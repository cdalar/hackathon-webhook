package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// reviewTimeout bounds one full review: fetching the diff, the Claude call,
// and posting the comment.
const reviewTimeout = 10 * time.Minute

// event is the subset of the Azure DevOps service hook payload we use.
type event struct {
	EventType string      `json:"eventType"`
	Resource  pullRequest `json:"resource"`
}

type pullRequest struct {
	ID            int    `json:"pullRequestId"`
	Status        string `json:"status"`
	Title         string `json:"title"`
	Description   string `json:"description"`
	SourceRefName string `json:"sourceRefName"`
	TargetRefName string `json:"targetRefName"`
	Repository    struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	} `json:"repository"`
	LastMergeSourceCommit struct {
		CommitID string `json:"commitId"`
	} `json:"lastMergeSourceCommit"`
	Reviewers []struct {
		ID          string `json:"id"`
		DisplayName string `json:"displayName"`
	} `json:"reviewers"`
}

func (pr pullRequest) hasReviewer(id string) bool {
	for _, r := range pr.Reviewers {
		if r.ID == id {
			return true
		}
	}
	return false
}

// prClient is the Azure DevOps side of a review.
type prClient interface {
	Diff(ctx context.Context, pr pullRequest) (prDiff, error)
	PostComment(ctx context.Context, pr pullRequest, markdown string) error
}

// reviewer turns a pull request and its diff into a markdown review.
type reviewer interface {
	Review(ctx context.Context, pr pullRequest, diff prDiff) (string, error)
}

type webhookHandler struct {
	aiReviewerID string
	secret       string
	prs          prClient
	reviewer     reviewer

	mu   sync.Mutex
	seen map[string]bool
	wg   sync.WaitGroup
}

func (h *webhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="webhook"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var ev event
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&ev); err != nil {
		http.Error(w, "invalid JSON payload", http.StatusBadRequest)
		return
	}
	pr := ev.Resource

	// Anything we don't act on still gets a 200: Azure DevOps disables a
	// subscription after repeated non-2xx responses.
	switch {
	case ev.EventType != "git.pullrequest.created" && ev.EventType != "git.pullrequest.updated":
		fmt.Fprintf(w, "ignored: event type %q\n", ev.EventType)
		return
	case pr.Status != "active":
		fmt.Fprintf(w, "ignored: pull request is %s\n", pr.Status)
		return
	case !pr.hasReviewer(h.aiReviewerID):
		fmt.Fprintln(w, "ignored: AI reviewer is not on the pull request")
		return
	}

	// The hook re-fires whenever the reviewer list changes while the AI
	// reviewer is on it, so review each source commit of a PR only once.
	key := fmt.Sprintf("%s/%d@%s", pr.Repository.ID, pr.ID, pr.LastMergeSourceCommit.CommitID)
	if !h.markSeen(key) {
		fmt.Fprintln(w, "ignored: already reviewed this commit")
		return
	}

	// Reviews take far longer than the service hook's delivery timeout, so
	// acknowledge now and do the work in the background.
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), reviewTimeout)
		defer cancel()
		if err := h.review(ctx, pr); err != nil {
			log.Printf("PR %d: review failed: %v", pr.ID, err)
			h.forget(key) // let a later delivery retry
			return
		}
		log.Printf("PR %d: review posted", pr.ID)
	}()

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintln(w, "review started")
}

func (h *webhookHandler) review(ctx context.Context, pr pullRequest) error {
	diff, err := h.prs.Diff(ctx, pr)
	if err != nil {
		return fmt.Errorf("fetching diff: %w", err)
	}
	review, err := h.reviewer.Review(ctx, pr, diff)
	if err != nil {
		return fmt.Errorf("reviewing: %w", err)
	}
	if err := h.prs.PostComment(ctx, pr, formatComment(pr, diff, review)); err != nil {
		return fmt.Errorf("posting comment: %w", err)
	}
	return nil
}

func formatComment(pr pullRequest, diff prDiff, review string) string {
	commit := pr.LastMergeSourceCommit.CommitID
	if len(commit) > 8 {
		commit = commit[:8]
	}
	comment := fmt.Sprintf("**🤖 AI Assistant review** (commit `%s`)\n\n%s", commit, review)
	if len(diff.Omitted) > 0 {
		comment += "\n\n---\n_Not reviewed:_\n"
		for _, o := range diff.Omitted {
			comment += "- " + o + "\n"
		}
	}
	return comment
}

// authorized checks the basic-auth password Azure DevOps sends with each
// delivery. The username is not checked.
func (h *webhookHandler) authorized(r *http.Request) bool {
	if h.secret == "" {
		return true
	}
	_, password, ok := r.BasicAuth()
	return ok && subtle.ConstantTimeCompare([]byte(password), []byte(h.secret)) == 1
}

func (h *webhookHandler) markSeen(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.seen[key] {
		return false
	}
	if h.seen == nil {
		h.seen = make(map[string]bool)
	}
	h.seen[key] = true
	return true
}

func (h *webhookHandler) forget(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.seen, key)
}

// wait blocks until all in-flight reviews have finished.
func (h *webhookHandler) wait() { h.wg.Wait() }
