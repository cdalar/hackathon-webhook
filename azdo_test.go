package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
			{"changeType":"edit","item":{"path":"/README.md","objectId":"new-readme","originalObjectId":"old-readme"}},
			{"changeType":"add","item":{"path":"/logo.png","objectId":"new-logo"}}]}`)
	})
	mux.HandleFunc("GET "+repo+"/blobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, blobs[r.PathValue("id")])
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
	if len(diff.Omitted) != 1 || diff.Omitted[0] != "/logo.png (binary file)" {
		t.Errorf("Omitted = %q, want the binary logo", diff.Omitted)
	}
}

func TestPostComment(t *testing.T) {
	var posted map[string]any
	srv := fakeAzdo(t, &posted)
	defer srv.Close()

	if err := newAzdoClient(srv.URL, "test-pat").PostComment(context.Background(), testPR(t), "**review**"); err != nil {
		t.Fatal(err)
	}
	comments, _ := posted["comments"].([]any)
	if len(comments) != 1 || comments[0].(map[string]any)["content"] != "**review**" {
		t.Errorf("posted thread = %v", posted)
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
