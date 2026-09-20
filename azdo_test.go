package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeAzdo serves the handful of Git REST endpoints the client uses.
func fakeAzdo(t *testing.T, posted *map[string]any) *httptest.Server {
	t.Helper()
	const repo = "/22222222-2222-2222-2222-222222222222/_apis/git/repositories/11111111-1111-1111-1111-111111111111"
	blobs := map[string]string{
		"old-readme": "# Demo\n",
		"new-readme": "# Demo\nHello!\n",
		"new-logo":   "PNG\x00\x01",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+repo+"/pullRequests/7/iterations", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"value":[{"id":1},{"id":2}]}`)
	})
	mux.HandleFunc("GET "+repo+"/pullRequests/7/iterations/2/changes", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"changeEntries":[
			{"changeTrackingId":5,"changeType":"edit","item":{"path":"/README.md","objectId":"new-readme","originalObjectId":"old-readme"}},
			{"changeType":"add","item":{"path":"/logo.png","objectId":"new-logo"}}]}`)
	})
	mux.HandleFunc("GET "+repo+"/blobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, blobs[r.PathValue("id")])
	})
	mux.HandleFunc("PATCH "+repo+"/pullrequests/7", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(posted); err != nil {
			t.Error(err)
		}
		fmt.Fprint(w, `{"pullRequestId":7}`)
	})
	mux.HandleFunc("POST "+repo+"/pullRequests/7/threads", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(posted); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":1}`)
	})
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, _ := r.BasicAuth(); user != "" || pass != "test-pat" {
			t.Errorf("%s %s: unexpected credentials %q:%q", r.Method, r.URL.Path, user, pass)
		}
		if got := r.URL.Query().Get("api-version"); got != apiVersion {
			t.Errorf("%s %s: api-version = %q", r.Method, r.URL.Path, got)
		}
		mux.ServeHTTP(w, r)
	}))
}

func testPR(t *testing.T) pullRequest {
	t.Helper()
	var ev event
	if err := json.Unmarshal([]byte(payload(t, nil)), &ev); err != nil {
		t.Fatal(err)
	}
	return ev.Resource
}

func TestDiff(t *testing.T) {
	srv := fakeAzdo(t, nil)
	defer srv.Close()

	diff, err := newAzdoClient(srv.URL, "test-pat").Diff(context.Background(), testPR(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# edit: /README.md", "--- a/README.md", "+++ b/README.md", "+Hello!"} {
		if !strings.Contains(diff.Text, want) {
			t.Errorf("diff missing %q:\n%s", want, diff.Text)
		}
	}
	if diff.Iteration != 2 || len(diff.Files) != 1 {
		t.Fatalf("Iteration = %d, Files = %+v, want iteration 2 and only the README", diff.Iteration, diff.Files)
	}
	if f := diff.Files[0]; f.Path != "/README.md" || f.ChangeTrackingID != 5 || !f.Lines[2] || !strings.Contains(f.Numbered, "    2 + Hello!") {
		t.Errorf("Files[0] = %+v", f)
	}
	if len(diff.Omitted) != 1 || diff.Omitted[0] != "/logo.png (add, binary file)" {
		t.Errorf("Omitted = %q, want the binary logo", diff.Omitted)
	}
}

func TestPostComment(t *testing.T) {
	var posted map[string]any
	srv := fakeAzdo(t, &posted)
	defer srv.Close()

	if err := newAzdoClient(srv.URL, "test-pat").PostComment(context.Background(), testPR(t), threadClosed, "**note**"); err != nil {
		t.Fatal(err)
	}
	comments, _ := posted["comments"].([]any)
	if len(comments) != 1 || comments[0].(map[string]any)["content"] != "**note**" {
		t.Errorf("posted thread = %v", posted)
	}
	if posted["status"] != float64(4) {
		t.Errorf("status = %v, want 4 (closed)", posted["status"])
	}
}

func TestUpdatePR(t *testing.T) {
	var sent map[string]any
	srv := fakeAzdo(t, &sent)
	defer srv.Close()

	if err := newAzdoClient(srv.URL, "test-pat").UpdatePR(context.Background(), testPR(t), "New title", "New description"); err != nil {
		t.Fatal(err)
	}
	if sent["title"] != "New title" || sent["description"] != "New description" || len(sent) != 2 {
		t.Errorf("PATCH body = %v, want only the new title and description", sent)
	}
}

func TestPostFileComment(t *testing.T) {
	file := fileChange{Path: "/README.md", ChangeTrackingID: 7, Lines: map[int]bool{2: true}}
	tests := map[string]struct {
		line     int
		wantLine bool
	}{
		"line in the diff anchors the thread":     {2, true},
		"unknown line comments on the whole file": {99, false},
		"line 0 comments on the whole file":       {0, false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var posted map[string]any
			srv := fakeAzdo(t, &posted)
			defer srv.Close()

			err := newAzdoClient(srv.URL, "test-pat").PostFileComment(context.Background(), testPR(t), 3, file, tc.line, "note")
			if err != nil {
				t.Fatal(err)
			}
			if posted["status"] != float64(1) {
				t.Errorf("status = %v, want 1 (active): a finding is for the author to resolve", posted["status"])
			}
			threadContext := posted["threadContext"].(map[string]any)
			if threadContext["filePath"] != "/README.md" {
				t.Errorf("filePath = %v", threadContext["filePath"])
			}
			start, hasLine := threadContext["rightFileStart"].(map[string]any)
			if hasLine != tc.wantLine || (hasLine && start["line"] != float64(tc.line)) {
				t.Errorf("rightFileStart = %v, want a line: %t", threadContext["rightFileStart"], tc.wantLine)
			}
			prContext := posted["pullRequestThreadContext"].(map[string]any)
			iterations := prContext["iterationContext"].(map[string]any)
			if prContext["changeTrackingId"] != float64(7) || iterations["secondComparingIteration"] != float64(3) {
				t.Errorf("pullRequestThreadContext = %v", prContext)
			}
		})
	}
}

func TestNumberedDiff(t *testing.T) {
	before := "one\ntwo\nthree\n"
	after := "one\n2\nthree\nfour" // no trailing newline
	got, lines := numberedDiff(before, after)
	want := "    1   one\n" +
		"      - two\n" +
		"    2 + 2\n" +
		"    3   three\n" +
		"    4 + four\n"
	if got != want {
		t.Errorf("numberedDiff =\n%s\nwant\n%s", got, want)
	}
	for line := 1; line <= 4; line++ {
		if !lines[line] {
			t.Errorf("line %d missing from the commentable set %v", line, lines)
		}
	}
	if lines[0] || lines[5] {
		t.Errorf("commentable set %v has lines that are not in the new file", lines)
	}
}

func TestErrorStatusIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNonAuthoritativeInfo) // what an invalid PAT gets
		fmt.Fprint(w, "<html>Sign In</html>")
	}))
	defer srv.Close()

	_, err := newAzdoClient(srv.URL, "bad").Diff(context.Background(), testPR(t))
	if err == nil || !strings.Contains(err.Error(), "203") {
		t.Errorf("err = %v, want a 203 error", err)
	}
}
