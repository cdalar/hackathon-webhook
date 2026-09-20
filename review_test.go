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

	got, err := newOpenAIClient(baseURL, os.Getenv("AI_MODEL"), os.Getenv("AI_API_KEY")).Review(ctx, pr, diff)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("diff shown to the model:\n%s", numbered)
	for _, c := range got {
		t.Logf("%s:%d (line shown: %t): %s", c.File, c.Line, lines[c.Line], c.Comment)
	}
	if len(got) == 0 {
		t.Errorf("no comments on a diff that introduces SQL injection and drops an error")
	}
}
