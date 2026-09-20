package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
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
		resp := map[string]any{
			"choices": []map[string]any{{
				"finish_reason": finish,
				"message":       map[string]string{"role": "assistant", "content": f.reply},
			}},
			"usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 20},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Error(err)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

var testDiff = prDiff{
	Iteration: 1,
	Text:      "# edit: /README.md\n--- a/README.md\n+++ b/README.md\n@@ -1 +1,2 @@\n # Demo\n+Hello!\n",
	Files: []fileChange{{
		Path: "/README.md", ChangeType: "edit", ChangeTrackingID: 1,
		Numbered: "    1   # Demo\n    2 + Hello!\n", Lines: map[int]bool{1: true, 2: true},
	}},
	Omitted: []string{"/logo.png (add, binary file)"},
}

func TestEnhance(t *testing.T) {
	ai := &fakeAI{reply: `{"title": "  Add greeting to README  ", "description": "Adds a greeting.\n"}`}
	srv := ai.start(t)

	got, err := newOpenAIClient(srv.URL+"/v1/", "my-model", "k3y").Enhance(context.Background(), testPR(t), testDiff)
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

	if _, err := newOpenAIClient(srv.URL+"/v1", "", "").Enhance(context.Background(), testPR(t), testDiff); err != nil {
		t.Fatal(err)
	}
	if ai.lastModel != "local-model" {
		t.Errorf("model = %q, want the first model the server lists", ai.lastModel)
	}
	if ai.lastAuth != "" {
		t.Errorf("Authorization = %q, want none without AI_API_KEY", ai.lastAuth)
	}
}

func TestThinkingBudgetAndExtraBody(t *testing.T) {
	reply := `{"title":"T","description":"D"}`

	// Unset: strict APIs reject unknown fields, so nothing extra may be sent.
	plain := &fakeAI{reply: reply}
	if _, err := newOpenAIClient(plain.start(t).URL+"/v1", "m", "").Enhance(context.Background(), testPR(t), testDiff); err != nil {
		t.Fatal(err)
	}
	if len(plain.lastBody) != 3 {
		t.Errorf("request fields = %v, want only model, messages, response_format", keys(plain.lastBody))
	}

	// A budget of 0 means "no thinking" and must be sent, unlike unset.
	capped := &fakeAI{reply: reply}
	client := newOpenAIClient(capped.start(t).URL+"/v1", "m", "")
	client.thinkingBudget = 0
	client.extraBody = map[string]any{
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
		"model":                "hijacked",
		"messages":             "hijacked",
	}
	if _, err := client.Enhance(context.Background(), testPR(t), testDiff); err != nil {
		t.Fatal(err)
	}
	if got, ok := capped.lastBody["thinking_budget_tokens"]; !ok || got != float64(0) {
		t.Errorf("thinking_budget_tokens = %v (sent: %t), want 0", got, ok)
	}
	if _, ok := capped.lastBody["chat_template_kwargs"].(map[string]any); !ok {
		t.Errorf("extra body field missing from request: %v", keys(capped.lastBody))
	}
	if capped.lastModel != "m" {
		t.Errorf("model = %q: the extra body must not replace core fields", capped.lastModel)
	}
	if _, ok := capped.lastBody["messages"].([]any); !ok {
		t.Errorf("messages = %v: the extra body must not replace core fields", capped.lastBody["messages"])
	}
}

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestEnhanceToleratesWrappedJSON(t *testing.T) {
	ai := &fakeAI{reply: "Here you go:\n```json\n{\"title\":\"T\",\"description\":\"uses {braces}\"}\n```"}
	srv := ai.start(t)

	got, err := newOpenAIClient(srv.URL+"/v1", "m", "").Enhance(context.Background(), testPR(t), testDiff)
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
			_, err := newOpenAIClient(srv.URL+"/v1", "m", "").Enhance(context.Background(), testPR(t), testDiff)
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

// liveClient builds a client for the live tests from the same environment
// variables the receiver reads, or skips the test if AI_BASE_URL is not set.
func liveClient(t *testing.T) *openAIClient {
	t.Helper()
	baseURL := os.Getenv("AI_BASE_URL")
	if baseURL == "" {
		t.Skip("AI_BASE_URL not set")
	}
	client := newOpenAIClient(baseURL, os.Getenv("AI_MODEL"), os.Getenv("AI_API_KEY"))
	if v := os.Getenv("AI_THINKING_BUDGET"); v != "" {
		budget, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("AI_THINKING_BUDGET: %v", err)
		}
		client.thinkingBudget = budget
	}
	return client
}

// TestEnhanceLive runs against a real OpenAI-compatible server:
//
//	AI_BASE_URL=http://my-llm-host:8080/v1 go test -run TestEnhanceLive -v .
func TestEnhanceLive(t *testing.T) {
	client := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pr := testPR(t)
	pr.Title, pr.Description = "fix", "see AB#1234"
	got, err := client.Enhance(ctx, pr, testDiff)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("title: %s\ndescription:\n%s", got.Title, got.Description)
	if !strings.Contains(got.Description, "AB#1234") {
		t.Errorf("description dropped the author's work item reference")
	}
}
