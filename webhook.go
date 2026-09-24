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

// runTimeout bounds one full run: fetching the diff, the AI calls, and
// writing the results back. Local models can take minutes on a large diff.
const runTimeout = 15 * time.Minute

// enhancedFooter is appended to every description we write. It tells readers
// where the text came from and marks the description as done, so it is never
// rewritten twice, even across restarts.
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
	PostComment(ctx context.Context, pr pullRequest, status threadStatus, markdown string) error
	PostFileComment(ctx context.Context, pr pullRequest, iteration int, file fileChange, firstLine, lastLine int, markdown string) error
	UpdatePR(ctx context.Context, pr pullRequest, title, description string) error
}

// enhancer turns a pull request and its diff into a better title and description.
type enhancer interface {
	Enhance(ctx context.Context, pr pullRequest, diff prDiff) (enhancement, error)
}

// reviewer turns a pull request's changed files into review comments.
type reviewer interface {
	Review(ctx context.Context, pr pullRequest, diff prDiff) ([]reviewComment, error)
}

type webhookHandler struct {
	aiReviewerID string
	secret       string
	mode         string
	prs          prClient
	enhancer     enhancer
	reviewer     reviewer // nil: don't comment on the changed files

	mu   sync.Mutex
	seen map[string]bool
	wg   sync.WaitGroup
}

func (h *webhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		log.Print("webhook: rejected: wrong or missing WEBHOOK_SECRET")
		w.Header().Set("WWW-Authenticate", `Basic realm="webhook"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var ev event
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&ev); err != nil {
		log.Printf("webhook: rejected: invalid JSON payload: %v", err)
		http.Error(w, "invalid JSON payload", http.StatusBadRequest)
		return
	}
	pr := ev.Resource

	// Anything we don't act on still gets a 200: Azure DevOps disables a
	// subscription after repeated non-2xx responses.
	if ev.EventType != "git.pullrequest.created" && ev.EventType != "git.pullrequest.updated" {
		log.Printf("webhook: ignored: event type %q", ev.EventType)
		fmt.Fprintf(w, "ignored: event type %q\n", ev.EventType)
		return
	}
	log.Printf("PR %d: %s event, repository %s, %s, commit %.7s, %d reviewer(s)", pr.ID, ev.EventType,
		pr.Repository.Name, pr.Status, pr.LastMergeSourceCommit.CommitID, len(pr.Reviewers))
	ignore := func(reason string) {
		log.Printf("PR %d: ignored: %s", pr.ID, reason)
		fmt.Fprintf(w, "ignored: %s\n", reason)
	}
	switch {
	case pr.Status != "active":
		ignore("pull request is " + pr.Status)
		return
	case !pr.hasReviewer(h.aiReviewerID):
		ignore("AI reviewer is not on the pull request")
		return
	case h.reviewer == nil && strings.Contains(pr.Description, enhancedFooter):
		ignore("pull request is already enhanced")
		return
	}

	// The hook re-fires whenever the reviewer list changes while the AI
	// reviewer is on it, so handle each source commit of a PR only once.
	key := fmt.Sprintf("%s/%d@%s", pr.Repository.ID, pr.ID, pr.LastMergeSourceCommit.CommitID)
	if !h.markSeen(key) {
		ignore("already handled this commit")
		return
	}
	log.Printf("PR %d: started", pr.ID)

	// The AI calls take far longer than the service hook's delivery timeout,
	// so acknowledge now and do the work in the background.
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
		defer cancel()
		started := time.Now()
		if err := h.run(ctx, pr); err != nil {
			log.Printf("PR %d: failed: %v (after %s)", pr.ID, err, time.Since(started).Round(time.Second))
			h.forget(key) // let a later delivery retry
			return
		}
		log.Printf("PR %d: done in %s", pr.ID, time.Since(started).Round(time.Second))
	}()

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintln(w, "enhancement started")
}

// run enhances the PR's title and description, unless that was done before,
// and then comments on the changed files.
func (h *webhookHandler) run(ctx context.Context, pr pullRequest) error {
	diff, err := h.prs.Diff(ctx, pr)
	if err != nil {
		return fmt.Errorf("fetching diff: %w", err)
	}
	log.Printf("PR %d: diff of iteration %d: %d file(s), %d KB, %d left out", pr.ID, diff.Iteration,
		len(diff.Files), len(diff.Text)/1024, len(diff.Omitted))
	if diff.Text == "" {
		log.Printf("PR %d: no text changes; commenting and stopping", pr.ID)
		return h.prs.PostComment(ctx, pr, threadClosed, "**🤖 AI Assistant:** this pull request has no text changes for me to read, so I left it as it is."+omittedNote(diff))
	}
	if !strings.Contains(pr.Description, enhancedFooter) {
		if err := h.enhance(ctx, pr, diff); err != nil {
			return err
		}
		log.Printf("PR %d: enhanced (%s mode)", pr.ID, h.mode)
	} else {
		log.Printf("PR %d: title and description already enhanced; skipping to the review", pr.ID)
	}
	if h.reviewer != nil {
		posted, err := h.review(ctx, pr, diff)
		if err != nil {
			return err
		}
		log.Printf("PR %d: reviewed, %d comments", pr.ID, posted)
	}
	return nil
}

func (h *webhookHandler) enhance(ctx context.Context, pr pullRequest, diff prDiff) error {
	result, err := h.enhancer.Enhance(ctx, pr, diff)
	if err != nil {
		return fmt.Errorf("asking the AI: %w", err)
	}

	if h.mode == modeSuggest {
		// Active on purpose: the suggestion is for the author to act on, and a
		// closed thread is collapsed out of sight.
		if err := h.prs.PostComment(ctx, pr, threadActive, suggestionComment(result, diff)); err != nil {
			return fmt.Errorf("posting suggestion: %w", err)
		}
		log.Printf("PR %d: posted suggested title %q", pr.ID, result.Title)
		return nil
	}

	// Save the author's text before overwriting it: Azure DevOps keeps no
	// history of a pull request's description. The thread is a record, not a
	// request, so it is born closed and can't hold up the PR.
	if err := h.prs.PostComment(ctx, pr, threadClosed, originalsComment(pr, diff)); err != nil {
		return fmt.Errorf("saving original description: %w", err)
	}
	log.Printf("PR %d: saved the original title and description in a comment", pr.ID)
	description := truncate(result.Description, maxDescriptionLen-len(enhancedFooter)-len("\n\n---\n")) +
		"\n\n---\n" + enhancedFooter
	if err := h.prs.UpdatePR(ctx, pr, result.Title, description); err != nil {
		return fmt.Errorf("updating pull request: %w", err)
	}
	log.Printf("PR %d: title %q -> %q, description rewritten", pr.ID, pr.Title, result.Title)
	return nil
}

// review posts the AI's comments on the changed files and returns how many.
func (h *webhookHandler) review(ctx context.Context, pr pullRequest, diff prDiff) (int, error) {
	comments, err := h.reviewer.Review(ctx, pr, diff)
	if err != nil {
		return 0, fmt.Errorf("asking the AI for a review: %w", err)
	}
	if len(comments) == 0 {
		log.Printf("PR %d: review found nothing to comment on", pr.ID)
		err := h.prs.PostComment(ctx, pr, threadClosed, fmt.Sprintf("**🤖 AI Assistant** reviewed the changes in %d file(s) and has no comments.%s",
			len(diff.Files), omittedNote(diff)))
		return 0, err
	}

	files := make(map[string]fileChange, len(diff.Files))
	for _, f := range diff.Files {
		files[strings.TrimPrefix(f.Path, "/")] = f
	}
	for i, c := range comments {
		var where string
		markdown := "**🤖 AI Assistant:** " + c.Comment
		if file, ok := files[strings.TrimPrefix(c.File, "/")]; ok {
			// A line the model got wrong becomes a comment on the whole file.
			lastLine := max(c.Line, c.EndLine)
			if block := suggestionBlock(file, c.Line, lastLine, c.Suggestion); block != "" {
				markdown += "\n\n" + block
			} else {
				lastLine = c.Line // without a suggestion, point at the one line
			}
			where = fmt.Sprintf("%s lines %d-%d", file.Path, c.Line, lastLine)
			if strings.Contains(markdown, "```suggestion") {
				where += ", with a suggested change"
			}
			err = h.prs.PostFileComment(ctx, pr, diff.Iteration, file, c.Line, lastLine, markdown)
		} else {
			where = c.File + ", which is not in the diff, so on the PR itself"
			err = h.prs.PostComment(ctx, pr, threadActive, fmt.Sprintf("**🤖 AI Assistant** on `%s`: %s", c.File, c.Comment))
		}
		if err != nil {
			return i, fmt.Errorf("posting review comment: %w", err)
		}
		log.Printf("PR %d: posted comment %d/%d on %s", pr.ID, i+1, len(comments), where)
	}
	return len(comments), nil
}

// maxSuggestionSpan is the most lines one suggestion may replace.
const maxSuggestionSpan = 20

// suggestionBlock renders replacement text for lines first through last of
// file as an Azure DevOps suggestion, which the author can apply with one
// click. It returns "" for a suggestion that is not safe to offer: no text, a
// range that is not wholly in the diff, a no-op, text that would break out of
// the code fence, or brackets that don't balance the way the replaced lines do.
func suggestionBlock(file fileChange, first, last int, suggestion string) string {
	suggestion = strings.Trim(suggestion, "\r\n")
	if strings.TrimSpace(suggestion) == "" || !file.hasLines(first, last) ||
		last-first >= maxSuggestionSpan || last > len(file.After) || strings.Contains(suggestion, "```") {
		return ""
	}
	replaced := strings.Join(file.After[first-1:last], "\n")
	repaired := reindent(suggestion, replaced)
	if repaired != suggestion && indentSensitive(file.Path) {
		return ""
	}
	suggestion = repaired
	if suggestion == replaced || bracketBalance(suggestion) != bracketBalance(replaced) {
		return ""
	}
	return "```suggestion\n" + suggestion + "\n```"
}

// reindent re-bases a suggestion on the indentation of the lines it replaces.
// Models tend to drop the leading whitespace of a reply's first line, or to
// write the whole block flush left while keeping its nesting; both come out
// right. A reply whose lines are indented inconsistently can't be repaired,
// only re-based, which is cosmetic in brace languages but not in others, so
// callers reject a changed result for indentation-sensitive files.
func reindent(suggestion, replaced string) string {
	base := ""
	for _, line := range strings.Split(replaced, "\n") {
		if strings.TrimSpace(line) != "" {
			base = leadingSpace(line)
			break
		}
	}
	lines := strings.Split(suggestion, "\n")

	// A first line with no indentation, above lines that share some, lost its
	// own; unless the code it replaces starts at column 0 too.
	if rest := commonIndent(lines[1:]); base != "" && rest != "" && leadingSpace(lines[0]) == "" {
		lines[0] = rest + lines[0]
	}
	own := commonIndent(lines)
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			lines[i] = ""
		} else {
			lines[i] = base + strings.TrimPrefix(line, own)
		}
	}
	return strings.Join(lines, "\n")
}

func leadingSpace(line string) string {
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}

// commonIndent is the longest leading whitespace shared by all non-blank lines.
func commonIndent(lines []string) string {
	common, seen := "", false
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := leadingSpace(line)
		if !seen {
			common, seen = indent, true
			continue
		}
		for !strings.HasPrefix(indent, common) {
			common = common[:len(common)-1]
		}
	}
	return common
}

// indentSensitive reports whether indentation carries meaning in the file, so
// that a suggestion whose indentation had to be repaired is not safe to offer.
func indentSensitive(path string) bool {
	switch strings.ToLower(path[strings.LastIndexByte(path, '.')+1:]) {
	case "py", "pyi", "yaml", "yml":
		return true
	}
	return false
}

// bracketBalance is the net count of opening minus closing brackets. A
// suggestion that closes a block its lines didn't open, or the reverse, would
// leave the file unbalanced. Brackets inside strings make this approximate; a
// mismatch only costs the suggestion, never the comment.
func bracketBalance(code string) (balance [3]int) {
	for _, r := range code {
		switch r {
		case '{':
			balance[0]++
		case '}':
			balance[0]--
		case '(':
			balance[1]++
		case ')':
			balance[1]--
		case '[':
			balance[2]++
		case ']':
			balance[2]--
		}
	}
	return balance
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
