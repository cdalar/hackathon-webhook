package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// enhanceTimeout bounds one full run: fetching the diff, the AI call, and
// writing the result back. Local models can take minutes on a large diff.
const enhanceTimeout = 10 * time.Minute

// enhancedFooter is appended to every description we write. It tells readers
// where the text came from and marks the PR as done, so it is never
// enhanced twice, even across restarts.
const enhancedFooter = "_Title and description enhanced by AI Assistant from this pull request's changes._"

const (
	modeUpdate  = "update"  // rewrite the PR's title and description in place
	modeSuggest = "suggest" // leave the PR alone and post the proposal as a comment
)

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

// prClient is the Azure DevOps side of an enhancement.
type prClient interface {
	Diff(ctx context.Context, pr pullRequest) (prDiff, error)
	PostComment(ctx context.Context, pr pullRequest, markdown string) error
	UpdatePR(ctx context.Context, pr pullRequest, title, description string) error
}

// enhancer turns a pull request and its diff into a better title and description.
type enhancer interface {
	Enhance(ctx context.Context, pr pullRequest, diff prDiff) (enhancement, error)
}

type webhookHandler struct {
	aiReviewerID string
	secret       string
	mode         string
	prs          prClient
	enhancer     enhancer

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
	case strings.Contains(pr.Description, enhancedFooter):
		fmt.Fprintln(w, "ignored: pull request is already enhanced")
		return
	}

	// The hook re-fires whenever the reviewer list changes while the AI
	// reviewer is on it, so handle each source commit of a PR only once.
	key := fmt.Sprintf("%s/%d@%s", pr.Repository.ID, pr.ID, pr.LastMergeSourceCommit.CommitID)
	if !h.markSeen(key) {
		fmt.Fprintln(w, "ignored: already handled this commit")
		return
	}

	// The AI call takes far longer than the service hook's delivery timeout,
	// so acknowledge now and do the work in the background.
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), enhanceTimeout)
		defer cancel()
		if err := h.enhance(ctx, pr); err != nil {
			log.Printf("PR %d: enhancement failed: %v", pr.ID, err)
			h.forget(key) // let a later delivery retry
			return
		}
		log.Printf("PR %d: enhanced (%s mode)", pr.ID, h.mode)
	}()

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintln(w, "enhancement started")
}

func (h *webhookHandler) enhance(ctx context.Context, pr pullRequest) error {
	diff, err := h.prs.Diff(ctx, pr)
	if err != nil {
		return fmt.Errorf("fetching diff: %w", err)
	}
	if diff.Text == "" {
		return h.prs.PostComment(ctx, pr, "**🤖 AI Assistant:** this pull request has no text changes to describe, so I left its title and description as they are.")
	}
	result, err := h.enhancer.Enhance(ctx, pr, diff)
	if err != nil {
		return fmt.Errorf("asking the AI: %w", err)
	}

	if h.mode == modeSuggest {
		if err := h.prs.PostComment(ctx, pr, suggestionComment(result, diff)); err != nil {
			return fmt.Errorf("posting suggestion: %w", err)
		}
		return nil
	}

	// Save the author's text before overwriting it: Azure DevOps keeps no
	// history of a pull request's description.
	if err := h.prs.PostComment(ctx, pr, originalsComment(pr, diff)); err != nil {
		return fmt.Errorf("saving original description: %w", err)
	}
	description := truncate(result.Description, maxDescriptionLen-len(enhancedFooter)-len("\n\n---\n")) +
		"\n\n---\n" + enhancedFooter
	if err := h.prs.UpdatePR(ctx, pr, result.Title, description); err != nil {
		return fmt.Errorf("updating pull request: %w", err)
	}
	return nil
}

func suggestionComment(result enhancement, diff prDiff) string {
	return fmt.Sprintf("**🤖 AI Assistant suggestion** for this pull request's title and description\n\n**Title:** %s\n\n**Description:**\n\n%s%s",
		result.Title, result.Description, omittedNote(diff))
}

func originalsComment(pr pullRequest, diff prDiff) string {
	original := "_(empty)_"
	if strings.TrimSpace(pr.Description) != "" {
		original = "> " + strings.ReplaceAll(strings.TrimSpace(pr.Description), "\n", "\n> ")
	}
	return fmt.Sprintf("**🤖 AI Assistant** rewrote this pull request's title and description from its changes. The author's originals are kept here.\n\n**Original title:** %s\n\n**Original description:**\n\n%s%s",
		pr.Title, original, omittedNote(diff))
}

func omittedNote(diff prDiff) string {
	if len(diff.Omitted) == 0 {
		return ""
	}
	return "\n\n---\n_Changes the AI did not see:_\n- " + strings.Join(diff.Omitted, "\n- ")
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

// wait blocks until all in-flight enhancements have finished.
func (h *webhookHandler) wait() { h.wg.Wait() }
