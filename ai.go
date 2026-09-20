package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// openAIClient calls any server that implements the OpenAI chat completions
// API: llama.cpp, Ollama, vLLM, LM Studio, or a hosted service.
type openAIClient struct {
	baseURL string // up to and including /v1
	model   string // empty: use the first model the server lists
	apiKey  string // empty: send no Authorization header
	http    *http.Client

	suggestions bool // ask reviews for one-click replacement code too
}

func newOpenAIClient(baseURL, model, apiKey string) *openAIClient {
	return &openAIClient{baseURL: strings.TrimRight(baseURL, "/"), model: model, apiKey: apiKey, http: &http.Client{}}
}

// chatJSON sends one system + user exchange and decodes the model's JSON
// answer, which must match schema, into out.
func (c *openAIClient) chatJSON(ctx context.Context, system, user, schemaName string, schema map[string]any, out any) error {
	model, err := c.resolveModel(ctx)
	if err != nil {
		return err
	}

	// No max_tokens or temperature: reasoning models spend an unpredictable
	// number of tokens thinking before they answer, and some hosted models
	// reject one or both parameters. The request context bounds the call.
	request := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"response_format": map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": schemaName, "strict": true, "schema": schema},
		},
	}
	var response struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := c.call(ctx, http.MethodPost, "/chat/completions", request, &response); err != nil {
		return err
	}
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
