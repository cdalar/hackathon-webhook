package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// fakeAI is an OpenAI-compatible server that answers every chat completion
// with reply, and records the last request it saw.
type fakeAI struct {
	reply        string
	finishReason string

	lastAuth  string
	lastModel string
	lastBody  map[string]any
}

func (f *fakeAI) start(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"local-model"},{"id":"other-model"}]}`)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		f.lastAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&f.lastBody); err != nil {
			t.Error(err)
		}
		f.lastModel, _ = f.lastBody["model"].(string)
		finish := f.finishReason
		if finish == "" {
			finish = "stop"
		}
		resp := map[string]any{"choices": []map[string]any{{
			"finish_reason": finish,
			"message":       map[string]string{"role": "assistant", "content": f.reply},
		}}}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Error(err)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

var testDiff = prDiff{
	Text:    "# edit: /README.md\n--- a/README.md\n+++ b/README.md\n@@ -1 +1,2 @@\n # Demo\n+Hello!\n",
	Omitted: []string{"/logo.png (add, binary file)"},
}

func TestEnhance(t *testing.T) {
	ai := &fakeAI{reply: `{"title": "  Add greeting to README  ", "description": "Adds a greeting.\n"}`}
	srv := ai.start(t)

	got, err := newOpenAIEnhancer(srv.URL+"/v1/", "my-model", "k3y").Enhance(context.Background(), testPR(t), testDiff)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Add greeting to README" || got.Description != "Adds a greeting." {
		t.Errorf("got %+v, want trimmed title and description", got)
	}
	if ai.lastAuth != "Bearer k3y" || ai.lastModel != "my-model" {
		t.Errorf("auth = %q, model = %q", ai.lastAuth, ai.lastModel)
	}

	// The prompt must carry everything the task asks the AI to read.
	messages := ai.lastBody["messages"].([]any)
	user := messages[len(messages)-1].(map[string]any)["content"].(string)
	for _, want := range []string{"Title: Add greeting", "Adds a greeting to the README.", "+Hello!", "/logo.png (add, binary file)"} {
		if !strings.Contains(user, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	for _, unwanted := range []string{"max_tokens", "temperature"} {
		if _, ok := ai.lastBody[unwanted]; ok {
			t.Errorf("request sets %s; reasoning and hosted models need it left out", unwanted)
		}
	}
}

func TestEnhanceDiscoversModelAndSkipsAuth(t *testing.T) {
	ai := &fakeAI{reply: `{"title":"T","description":"D"}`}
	srv := ai.start(t)

	if _, err := newOpenAIEnhancer(srv.URL+"/v1", "", "").Enhance(context.Background(), testPR(t), testDiff); err != nil {
		t.Fatal(err)
	}
	if ai.lastModel != "local-model" {
		t.Errorf("model = %q, want the first model the server lists", ai.lastModel)
	}
	if ai.lastAuth != "" {
		t.Errorf("Authorization = %q, want none without AI_API_KEY", ai.lastAuth)
	}
}

func TestEnhanceToleratesWrappedJSON(t *testing.T) {
	ai := &fakeAI{reply: "Here you go:\n```json\n{\"title\":\"T\",\"description\":\"uses {braces}\"}\n```"}
	srv := ai.start(t)

	got, err := newOpenAIEnhancer(srv.URL+"/v1", "m", "").Enhance(context.Background(), testPR(t), testDiff)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "T" || got.Description != "uses {braces}" {
		t.Errorf("got %+v", got)
	}
}

func TestEnhanceErrors(t *testing.T) {
	tests := map[string]struct {
		ai   fakeAI
		want string
	}{
		"cut off while thinking": {fakeAI{reply: "", finishReason: "length"}, "token limit"},
		"not JSON":               {fakeAI{reply: "Sorry, I can't help with that."}, "not JSON"},
		"empty fields":           {fakeAI{reply: `{"title":"","description":"D"}`}, "empty title"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			srv := tc.ai.start(t)
			_, err := newOpenAIEnhancer(srv.URL+"/v1", "m", "").Enhance(context.Background(), testPR(t), testDiff)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate left a short string as %q", got)
	}
	got := truncate(strings.Repeat("é", 10), 9) // 2 bytes per rune
	if len(got) > 9 || !strings.HasSuffix(got, "…") || !utf8.ValidString(got) {
		t.Errorf("truncate = %q (%d bytes), want valid UTF-8 within 9 bytes ending in …", got, len(got))
	}
}

// TestEnhanceLive runs against a real OpenAI-compatible server:
//
//	AI_BASE_URL=http://my-llm-host:8080/v1 go test -run TestEnhanceLive -v .
func TestEnhanceLive(t *testing.T) {
	baseURL := os.Getenv("AI_BASE_URL")
	if baseURL == "" {
		t.Skip("AI_BASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pr := testPR(t)
	pr.Title, pr.Description = "fix", "see AB#1234"
	got, err := newOpenAIEnhancer(baseURL, os.Getenv("AI_MODEL"), os.Getenv("AI_API_KEY")).Enhance(ctx, pr, testDiff)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("title: %s\ndescription:\n%s", got.Title, got.Description)
	if !strings.Contains(got.Description, "AB#1234") {
		t.Errorf("description dropped the author's work item reference")
	}
}
