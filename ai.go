package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// openAIClient calls any server that implements the OpenAI chat completions
// API: llama.cpp, Ollama, vLLM, LM Studio, or a hosted service.
type openAIClient struct {
	baseURL string // up to and including /v1
	model   string // empty: use the first model the server lists
	apiKey  string // empty: send no Authorization header
	http    *http.Client

	suggestions bool // ask reviews for one-click replacement code too

	// thinkingBudget caps a reasoning model's hidden thinking, in tokens, via
	// llama.cpp's thinking_budget_tokens. Negative: no cap, field not sent.
	thinkingBudget int
	// extraBody is merged into every chat request, for server-specific
	// options. It cannot replace the fields chatJSON sets itself.
	extraBody map[string]any
}

func newOpenAIClient(baseURL, model, apiKey string) *openAIClient {
	return &openAIClient{baseURL: strings.TrimRight(baseURL, "/"), model: model, apiKey: apiKey, http: &http.Client{}, thinkingBudget: -1}
}

// chatJSON sends one system + user exchange and decodes the model's JSON
// answer, which must match schema, into out. label names the call in the log.
func (c *openAIClient) chatJSON(ctx context.Context, label, system, user, schemaName string, schema map[string]any, out any) error {
	model, err := c.resolveModel(ctx)
	if err != nil {
		return err
	}

	// No max_tokens or temperature: reasoning models spend an unpredictable
	// number of tokens thinking before they answer, and some hosted models
	// reject one or both parameters. The request context bounds the call.
	request := make(map[string]any, len(c.extraBody)+4)
	for k, v := range c.extraBody {
		request[k] = v
	}
	if c.thinkingBudget >= 0 {
		request["thinking_budget_tokens"] = c.thinkingBudget
	}
	request["model"] = model
	request["messages"] = []map[string]string{
		{"role": "system", "content": system},
		{"role": "user", "content": user},
	}
	request["response_format"] = map[string]any{
		"type":        "json_schema",
		"json_schema": map[string]any{"name": schemaName, "strict": true, "schema": schema},
	}
	var response struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	log.Printf("%s: asking %s (%d KB prompt)", label, model, (len(system)+len(user)+512)/1024)
	started := time.Now()
	if err := c.call(ctx, http.MethodPost, "/chat/completions", request, &response); err != nil {
		log.Printf("%s: AI call failed after %s", label, time.Since(started).Round(time.Second))
		return err
	}
	// Completion tokens include hidden thinking, so they show what a slow
	// answer was spent on.
	log.Printf("%s: answered in %s (%d prompt + %d completion tokens)", label,
		time.Since(started).Round(time.Second), response.Usage.PromptTokens, response.Usage.CompletionTokens)
	if len(response.Choices) == 0 {
		return errors.New("AI server returned no choices")
	}
	choice := response.Choices[0]
	if choice.FinishReason == "length" {
		return errors.New("AI server cut the answer off at its token limit")
	}

	// Servers that ignore response_format tend to wrap the JSON in a code
	// fence or a sentence, so parse from the first brace to the last.
	content := choice.Message.Content
	start, end := strings.IndexByte(content, '{'), strings.LastIndexByte(content, '}')
	if start < 0 || end < start {
		return fmt.Errorf("AI reply is not JSON: %.120q", content)
	}
	if err := json.Unmarshal([]byte(content[start:end+1]), out); err != nil {
		return fmt.Errorf("AI reply is not valid JSON: %w", err)
	}
	return nil
}

func (c *openAIClient) resolveModel(ctx context.Context) (string, error) {
	if c.model != "" {
		return c.model, nil
	}
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.call(ctx, http.MethodGet, "/models", nil, &models); err != nil {
		return "", err
	}
	if len(models.Data) == 0 {
		return "", errors.New("AI server lists no models; set AI_MODEL")
	}
	return models.Data[0].ID, nil
}

func (c *openAIClient) call(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("AI server: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("AI server: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("AI server: %s %s: %s: %.200s", method, path, resp.Status,
			strings.Join(strings.Fields(string(data)), " "))
	}
	return json.Unmarshal(data, out)
}
