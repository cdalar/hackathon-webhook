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

type fakePRs struct {
	mu       sync.Mutex
	comments []string
}

func (f *fakePRs) Diff(context.Context, pullRequest) (prDiff, error) {
	return prDiff{Text: "--- a/README.md\n+++ b/README.md\n", Omitted: []string{"/logo.png (binary file)"}}, nil
}

func (f *fakePRs) PostComment(_ context.Context, _ pullRequest, markdown string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, markdown)
	return nil
}

type fakeReviewer struct {
	err   error
	calls int
}

func (f *fakeReviewer) Review(context.Context, pullRequest, prDiff) (string, error) {
	f.calls++
	return "Looks good.", f.err
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

func TestReviewPostedWhenAIReviewerPresent(t *testing.T) {
	prs, rev := &fakePRs{}, &fakeReviewer{}
	h := &webhookHandler{aiReviewerID: testReviewerID, prs: prs, reviewer: rev}

	rec := deliver(h, payload(t, nil), "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if len(prs.comments) != 1 {
		t.Fatalf("posted %d comments, want 1", len(prs.comments))
	}
	for _, want := range []string{"Looks good.", "`628b4fa0`", "/logo.png (binary file)"} {
		if !strings.Contains(prs.comments[0], want) {
			t.Errorf("comment missing %q:\n%s", want, prs.comments[0])
		}
	}

	// The hook re-fires on later reviewer changes; the same commit is reviewed once.
	if rec := deliver(h, payload(t, nil), ""); rec.Code != http.StatusOK {
		t.Errorf("duplicate delivery status = %d, want 200", rec.Code)
	}
	if rev.calls != 1 {
		t.Errorf("reviewer called %d times, want 1", rev.calls)
	}
}

func TestIgnoredDeliveries(t *testing.T) {
	tests := map[string]func(*event){
		"AI reviewer not on PR": func(ev *event) { ev.Resource.Reviewers = ev.Resource.Reviewers[1:] },
		"PR not active":         func(ev *event) { ev.Resource.Status = "completed" },
		"other event type":      func(ev *event) { ev.EventType = "git.push" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			rev := &fakeReviewer{}
			h := &webhookHandler{aiReviewerID: testReviewerID, prs: &fakePRs{}, reviewer: rev}
			rec := deliver(h, payload(t, mutate), "")
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rec.Code)
			}
			if rev.calls != 0 {
				t.Errorf("reviewer called %d times, want 0", rev.calls)
			}
		})
	}
}

func TestWebhookSecret(t *testing.T) {
	rev := &fakeReviewer{}
	h := &webhookHandler{aiReviewerID: testReviewerID, secret: "s3cret", prs: &fakePRs{}, reviewer: rev}

	if rec := deliver(h, payload(t, nil), ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no credentials: status = %d, want 401", rec.Code)
	}
	if rec := deliver(h, payload(t, nil), "wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong password: status = %d, want 401", rec.Code)
	}
	if rev.calls != 0 {
		t.Fatalf("reviewer called %d times before authenticating, want 0", rev.calls)
	}
	if rec := deliver(h, payload(t, nil), "s3cret"); rec.Code != http.StatusAccepted {
		t.Errorf("correct password: status = %d, want 202", rec.Code)
	}
}

func TestFailedReviewCanBeRetried(t *testing.T) {
	prs, rev := &fakePRs{}, &fakeReviewer{err: errors.New("boom")}
	h := &webhookHandler{aiReviewerID: testReviewerID, prs: prs, reviewer: rev}

	deliver(h, payload(t, nil), "")
	if len(prs.comments) != 0 {
		t.Fatalf("posted %d comments after a failed review, want 0", len(prs.comments))
	}
	rev.err = nil
	if rec := deliver(h, payload(t, nil), ""); rec.Code != http.StatusAccepted {
		t.Errorf("retry status = %d, want 202", rec.Code)
	}
	if len(prs.comments) != 1 {
		t.Errorf("posted %d comments after retry, want 1", len(prs.comments))
	}
}
