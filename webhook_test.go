package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

const testReviewerID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

type prUpdate struct{ title, description string }

type fileComment struct {
	path      string
	line      int
	iteration int
	markdown  string
}

type fakePRs struct {
	diff prDiff

	mu           sync.Mutex
	comments     []string
	statuses     []threadStatus // parallel to comments
	fileComments []fileComment
	updates      []prUpdate
}

func newFakePRs() *fakePRs {
	return &fakePRs{diff: prDiff{
		Iteration: 2,
		Text:      "--- a/README.md\n+++ b/README.md\n",
		Files:     []fileChange{{Path: "/README.md", ChangeType: "edit", ChangeTrackingID: 1, Lines: map[int]bool{1: true, 2: true}}},
		Omitted:   []string{"/logo.png (add, binary file)"},
	}}
}

func (f *fakePRs) Diff(context.Context, pullRequest) (prDiff, error) { return f.diff, nil }

func (f *fakePRs) PostComment(_ context.Context, _ pullRequest, status threadStatus, markdown string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, markdown)
	f.statuses = append(f.statuses, status)
	return nil
}

func (f *fakePRs) PostFileComment(_ context.Context, _ pullRequest, iteration int, file fileChange, line int, markdown string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fileComments = append(f.fileComments, fileComment{file.Path, line, iteration, markdown})
	return nil
}

func (f *fakePRs) UpdatePR(_ context.Context, _ pullRequest, title, description string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, prUpdate{title, description})
	return nil
}

type fakeEnhancer struct {
	result enhancement
	err    error
	calls  int
}

func newFakeEnhancer() *fakeEnhancer {
	return &fakeEnhancer{result: enhancement{Title: "Add greeting to README", Description: "Adds a greeting."}}
}

func (f *fakeEnhancer) Enhance(context.Context, pullRequest, prDiff) (enhancement, error) {
	f.calls++
	return f.result, f.err
}

type fakeReviewer struct {
	comments []reviewComment
	err      error
	calls    int
}

func (f *fakeReviewer) Review(context.Context, pullRequest, prDiff) ([]reviewComment, error) {
	f.calls++
	return f.comments, f.err
}

func payload(t *testing.T, mutate func(*event)) string {
	t.Helper()
	data, err := os.ReadFile("testdata/pullrequest_updated.json")
	if err != nil {
		t.Fatal(err)
	}
	if mutate == nil {
		return string(data)
	}
	// Round-tripping through event drops fields we don't use, which is fine.
	var ev event
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatal(err)
	}
	mutate(&ev)
	out, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func deliver(h *webhookHandler, body, password string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	if password != "" {
		req.SetBasicAuth("azdo", password)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	h.wait()
	return rec
}

func TestUpdateModeRewritesPRAndKeepsOriginals(t *testing.T) {
	prs, enh := newFakePRs(), newFakeEnhancer()
	h := &webhookHandler{aiReviewerID: testReviewerID, mode: modeUpdate, prs: prs, enhancer: enh}

	if rec := deliver(h, payload(t, nil), ""); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}

	if len(prs.updates) != 1 {
		t.Fatalf("updated the PR %d times, want 1", len(prs.updates))
	}
	got := prs.updates[0]
	if got.title != "Add greeting to README" {
		t.Errorf("title = %q", got.title)
	}
	if !strings.HasPrefix(got.description, "Adds a greeting.") || !strings.HasSuffix(got.description, enhancedFooter) {
		t.Errorf("description = %q, want the AI text followed by the footer", got.description)
	}

	if len(prs.comments) != 1 {
		t.Fatalf("posted %d comments, want 1", len(prs.comments))
	}
	if prs.statuses[0] != threadClosed {
		t.Errorf("originals thread status = %d; it must be born closed so it can't block the PR", prs.statuses[0])
	}
	for _, want := range []string{"**Original title:** Add greeting", "> Adds a greeting to the README.", "/logo.png (add, binary file)"} {
		if !strings.Contains(prs.comments[0], want) {
			t.Errorf("originals comment missing %q:\n%s", want, prs.comments[0])
		}
	}

	// The hook re-fires on later reviewer changes; the same commit is handled once.
	if rec := deliver(h, payload(t, nil), ""); rec.Code != http.StatusOK {
		t.Errorf("duplicate delivery status = %d, want 200", rec.Code)
	}
	if enh.calls != 1 {
		t.Errorf("enhancer called %d times, want 1", enh.calls)
	}
}

func TestSuggestModeOnlyComments(t *testing.T) {
	prs := newFakePRs()
	h := &webhookHandler{aiReviewerID: testReviewerID, mode: modeSuggest, prs: prs, enhancer: newFakeEnhancer()}

	deliver(h, payload(t, nil), "")
	if len(prs.updates) != 0 {
		t.Errorf("suggest mode updated the PR: %+v", prs.updates)
	}
	if len(prs.comments) != 1 || !strings.Contains(prs.comments[0], "**Title:** Add greeting to README") {
		t.Fatalf("comments = %q, want one suggestion", prs.comments)
	}
	if prs.statuses[0] != threadActive {
		t.Errorf("suggestion thread status = %d, want active: it is for the author to act on", prs.statuses[0])
	}
}

func TestLongDescriptionFitsAzureDevOpsLimit(t *testing.T) {
	prs, enh := newFakePRs(), newFakeEnhancer()
	enh.result.Description = strings.Repeat("långt ", 1000) // multi-byte, well over the limit
	h := &webhookHandler{aiReviewerID: testReviewerID, mode: modeUpdate, prs: prs, enhancer: enh}

	deliver(h, payload(t, nil), "")
	got := prs.updates[0].description
	if len(got) > maxDescriptionLen {
		t.Errorf("description is %d bytes, limit is %d", len(got), maxDescriptionLen)
	}
	if !strings.HasSuffix(got, enhancedFooter) || !strings.Contains(got, "…") {
		t.Errorf("truncated description lost its ellipsis or footer: …%q", got[len(got)-120:])
	}
}

func TestNoTextChangesLeavesPRAlone(t *testing.T) {
	prs, enh := newFakePRs(), newFakeEnhancer()
	prs.diff = prDiff{Omitted: []string{"/logo.png (add, binary file)"}}
	h := &webhookHandler{aiReviewerID: testReviewerID, mode: modeUpdate, prs: prs, enhancer: enh}

	deliver(h, payload(t, nil), "")
	if enh.calls != 0 || len(prs.updates) != 0 {
		t.Errorf("enhancer calls = %d, updates = %d, want none", enh.calls, len(prs.updates))
	}
	if len(prs.comments) != 1 || prs.statuses[0] != threadClosed {
		t.Errorf("comments = %q statuses = %v, want 1 closed note explaining why", prs.comments, prs.statuses)
	}
}

func TestIgnoredDeliveries(t *testing.T) {
	tests := map[string]func(*event){
		"AI reviewer not on PR": func(ev *event) { ev.Resource.Reviewers = ev.Resource.Reviewers[1:] },
		"PR not active":         func(ev *event) { ev.Resource.Status = "completed" },
		"other event type":      func(ev *event) { ev.EventType = "git.push" },
		"already enhanced":      func(ev *event) { ev.Resource.Description += "\n\n---\n" + enhancedFooter },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			enh := newFakeEnhancer()
			h := &webhookHandler{aiReviewerID: testReviewerID, mode: modeUpdate, prs: newFakePRs(), enhancer: enh}
			rec := deliver(h, payload(t, mutate), "")
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rec.Code)
			}
			if enh.calls != 0 {
				t.Errorf("enhancer called %d times, want 0", enh.calls)
			}
		})
	}
}

func TestWebhookSecret(t *testing.T) {
	enh := newFakeEnhancer()
	h := &webhookHandler{aiReviewerID: testReviewerID, secret: "s3cret", mode: modeUpdate, prs: newFakePRs(), enhancer: enh}

	if rec := deliver(h, payload(t, nil), ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no credentials: status = %d, want 401", rec.Code)
	}
	if rec := deliver(h, payload(t, nil), "wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong password: status = %d, want 401", rec.Code)
	}
	if enh.calls != 0 {
		t.Fatalf("enhancer called %d times before authenticating, want 0", enh.calls)
	}
	if rec := deliver(h, payload(t, nil), "s3cret"); rec.Code != http.StatusAccepted {
		t.Errorf("correct password: status = %d, want 202", rec.Code)
	}
}

func TestFailedEnhancementCanBeRetried(t *testing.T) {
	prs, enh := newFakePRs(), newFakeEnhancer()
	enh.err = errors.New("boom")
	h := &webhookHandler{aiReviewerID: testReviewerID, mode: modeUpdate, prs: prs, enhancer: enh}

	deliver(h, payload(t, nil), "")
	if len(prs.comments) != 0 || len(prs.updates) != 0 {
		t.Fatalf("a failed enhancement touched the PR: comments=%d updates=%d", len(prs.comments), len(prs.updates))
	}
	enh.err = nil
	if rec := deliver(h, payload(t, nil), ""); rec.Code != http.StatusAccepted {
		t.Errorf("retry status = %d, want 202", rec.Code)
	}
	if len(prs.updates) != 1 {
		t.Errorf("updated the PR %d times after retry, want 1", len(prs.updates))
	}
}

func TestReviewCommentsArePostedOnFiles(t *testing.T) {
	prs := newFakePRs()
	rev := &fakeReviewer{comments: []reviewComment{
		{File: "/README.md", Line: 2, Comment: "Typo in the greeting."},
		{File: "README.md", Line: 99, Comment: "Line the model made up."},
		{File: "/missing.go", Line: 1, Comment: "File that is not in the diff."},
	}}
	h := &webhookHandler{aiReviewerID: testReviewerID, mode: modeUpdate, prs: prs, enhancer: newFakeEnhancer(), reviewer: rev}

	if rec := deliver(h, payload(t, nil), ""); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if len(prs.updates) != 1 {
		t.Errorf("updated the PR %d times, want 1: review must not replace the enhancement", len(prs.updates))
	}

	if len(prs.fileComments) != 2 {
		t.Fatalf("posted %d file comments, want 2: %+v", len(prs.fileComments), prs.fileComments)
	}
	first, second := prs.fileComments[0], prs.fileComments[1]
	if first.path != "/README.md" || first.line != 2 || first.iteration != 2 || !strings.Contains(first.markdown, "Typo in the greeting.") {
		t.Errorf("first file comment = %+v", first)
	}
	if second.path != "/README.md" || second.line != 99 {
		t.Errorf("second file comment = %+v, want it passed on for the client to make file-level", second)
	}

	// originals comment + the comment for the file that is not in the diff
	if len(prs.comments) != 2 || !strings.Contains(prs.comments[1], "`/missing.go`") {
		t.Fatalf("general comments = %q", prs.comments)
	}
	if prs.statuses[1] != threadActive {
		t.Errorf("review finding status = %d, want active", prs.statuses[1])
	}
}

func TestReviewWithNothingToSaySaysSo(t *testing.T) {
	prs := newFakePRs()
	h := &webhookHandler{aiReviewerID: testReviewerID, mode: modeSuggest, prs: prs, enhancer: newFakeEnhancer(), reviewer: &fakeReviewer{}}

	deliver(h, payload(t, nil), "")
	if len(prs.fileComments) != 0 {
		t.Errorf("file comments = %+v, want none", prs.fileComments)
	}
	if len(prs.comments) != 2 || !strings.Contains(prs.comments[1], "has no comments") {
		t.Fatalf("comments = %q, want the suggestion and a no-comments note", prs.comments)
	}
	if prs.statuses[1] != threadClosed {
		t.Errorf("no-comments note status = %d, want closed", prs.statuses[1])
	}
}

func TestEnhancedPRIsStillReviewed(t *testing.T) {
	prs, enh, rev := newFakePRs(), newFakeEnhancer(), &fakeReviewer{comments: []reviewComment{{File: "/README.md", Line: 1, Comment: "x"}}}
	h := &webhookHandler{aiReviewerID: testReviewerID, mode: modeUpdate, prs: prs, enhancer: enh, reviewer: rev}

	deliver(h, payload(t, func(ev *event) { ev.Resource.Description += "\n\n---\n" + enhancedFooter }), "")
	if enh.calls != 0 || len(prs.updates) != 0 {
		t.Errorf("enhancer calls = %d, updates = %d: an enhanced description must not be rewritten", enh.calls, len(prs.updates))
	}
	if rev.calls != 1 || len(prs.fileComments) != 1 {
		t.Errorf("reviewer calls = %d, file comments = %d, want 1 and 1", rev.calls, len(prs.fileComments))
	}
}
