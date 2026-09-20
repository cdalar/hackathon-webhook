package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestReview(t *testing.T) {
	ai := &fakeAI{reply: `{"comments":[
		{"file":"/README.md","line":2,"comment":"  Greeting has no punctuation.  "},
		{"file":"/README.md","line":0,"comment":""}]}`}
	srv := ai.start(t)

	got, err := newOpenAIClient(srv.URL+"/v1", "m", "").Review(context.Background(), testPR(t), testDiff)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != (reviewComment{File: "/README.md", Line: 2, Comment: "Greeting has no punctuation."}) {
		t.Errorf("comments = %+v, want the one non-empty comment, trimmed", got)
	}

	// The model can only cite lines it was shown, numbered.
	messages := ai.lastBody["messages"].([]any)
	user := messages[len(messages)-1].(map[string]any)["content"].(string)
	for _, want := range []string{`<file path="/README.md" change="edit">`, "    2 + Hello!", "/logo.png (add, binary file)"} {
		if !strings.Contains(user, want) {
			t.Errorf("prompt missing %q:\n%s", want, user)
		}
	}
}

func TestReviewSuggestionsAreOptIn(t *testing.T) {
	reply := `{"comments":[{"file":"/README.md","line":2,"end_line":2,"comment":"Typo.","suggestion":"Hello!"}]}`
	schemaFields := func(ai *fakeAI) map[string]any {
		format := ai.lastBody["response_format"].(map[string]any)["json_schema"].(map[string]any)["schema"].(map[string]any)
		return format["properties"].(map[string]any)["comments"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	}
	systemPrompt := func(ai *fakeAI) string {
		return ai.lastBody["messages"].([]any)[0].(map[string]any)["content"].(string)
	}

	off := &fakeAI{reply: reply}
	got, err := newOpenAIClient(off.start(t).URL+"/v1", "m", "").Review(context.Background(), testPR(t), testDiff)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Suggestion != "" {
		t.Errorf("suggestion = %q with suggestions off; a server ignoring the schema must not sneak one in", got[0].Suggestion)
	}
	if _, asked := schemaFields(off)["suggestion"]; asked || strings.Contains(systemPrompt(off), "suggestion") {
		t.Errorf("suggestions off, but the request still asks for them")
	}

	on := &fakeAI{reply: reply}
	client := newOpenAIClient(on.start(t).URL+"/v1", "m", "")
	client.suggestions = true
	got, err = client.Review(context.Background(), testPR(t), testDiff)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Suggestion != "Hello!" || got[0].EndLine != 2 {
		t.Errorf("comment = %+v, want the suggestion and its range", got[0])
	}
	if _, asked := schemaFields(on)["suggestion"]; !asked || !strings.Contains(systemPrompt(on), `"suggestion"`) {
		t.Errorf("suggestions on, but the request does not ask for them")
	}
}

func TestReviewCapsComments(t *testing.T) {
	var items []string
	for i := 0; i < maxReviewComments+5; i++ {
		items = append(items, fmt.Sprintf(`{"file":"/README.md","line":1,"comment":"c%d"}`, i))
	}
	ai := &fakeAI{reply: `{"comments":[` + strings.Join(items, ",") + `]}`}
	srv := ai.start(t)

	got, err := newOpenAIClient(srv.URL+"/v1", "m", "").Review(context.Background(), testPR(t), testDiff)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != maxReviewComments || got[0].Comment != "c0" {
		t.Errorf("got %d comments starting with %q, want the first %d", len(got), got[0].Comment, maxReviewComments)
	}
}

// TestReviewLive runs against a real OpenAI-compatible server:
//
//	AI_BASE_URL=http://my-llm-host:8080/v1 go test -run TestReviewLive -v .
func TestReviewLive(t *testing.T) {
	baseURL := os.Getenv("AI_BASE_URL")
	if baseURL == "" {
		t.Skip("AI_BASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	before := "package pay\n\nfunc Refund(db *sql.DB, id string) error {\n\t_, err := db.Exec(\"UPDATE orders SET refunded = true WHERE id = ?\", id)\n\treturn err\n}\n"
	after := "package pay\n\nfunc Refund(db *sql.DB, id string) error {\n\tdb.Exec(\"UPDATE orders SET refunded = true WHERE id = '\" + id + \"'\")\n\treturn nil\n}\n"
	numbered, lines := numberedDiff(before, after)
	diff := prDiff{Iteration: 1, Files: []fileChange{{Path: "/pay/refund.go", ChangeType: "edit", Numbered: numbered, Lines: lines}}}
	pr := testPR(t)
	pr.Title, pr.Description = "Simplify refund query", "Small cleanup."

	file := fileChange{Path: "/pay/refund.go", Lines: lines, After: strings.Split(strings.TrimSuffix(after, "\n"), "\n")}
	client := newOpenAIClient(baseURL, os.Getenv("AI_MODEL"), os.Getenv("AI_API_KEY"))
	client.suggestions = true
	got, err := client.Review(ctx, pr, diff)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("diff shown to the model:\n%s", numbered)
	for _, c := range got {
		t.Logf("%s:%d-%d (line shown: %t): %s", c.File, c.Line, c.EndLine, lines[c.Line], c.Comment)
		if block := suggestionBlock(file, c.Line, max(c.Line, c.EndLine), c.Suggestion); block != "" {
			t.Logf("accepted suggestion:\n%s", block)
		} else if c.Suggestion != "" {
			t.Logf("REJECTED suggestion: %q", c.Suggestion)
		}
	}
	if len(got) == 0 {
		t.Errorf("no comments on a diff that introduces SQL injection and drops an error")
	}
}
