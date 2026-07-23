package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// DefaultBaseURL is OpenRouter's OpenAI-compatible endpoint.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

// DefaultModel is a free, tool-capable model used until an administrator picks
// one. It is only a starting point; the model list is fetched live.
const DefaultModel = "openrouter/free"

// Client talks to OpenRouter's chat completions API.
type Client struct {
	APIKey  string
	Model   string
	BaseURL string
	HTTP    *http.Client

	// Referer and Title are sent as the attribution headers OpenRouter uses on
	// its dashboard. They are optional.
	Referer string
	Title   string
}

// NewClient builds a client with sane defaults.
func NewClient(apiKey, model string) *Client {
	if model == "" {
		model = DefaultModel
	}
	return &Client{
		APIKey:  strings.TrimSpace(apiKey),
		Model:   model,
		BaseURL: DefaultBaseURL,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
		Referer: "https://ebdaa.me",
		Title:   "HAProxy Controller",
	}
}

// Role constants for chat messages.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Message is one chat message in the OpenAI-compatible schema.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is a function call the model asked to make.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall carries the name and raw JSON arguments of a tool call.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool is a function the model may call, in the OpenAI tool schema.
type Tool struct {
	Type     string             `json:"type"`
	Function FunctionDefinition `json:"function"`
}

// FunctionDefinition describes a callable tool.
type FunctionDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// chatRequest is the request body for a completion.
type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	ToolChoice  string    `json:"tool_choice,omitempty"`
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
}

// chatResponse is the relevant part of a completion response.
type chatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *apiError `json:"error"`
}

type apiError struct {
	Message string `json:"message"`
	Code    any    `json:"code"`
}

// Completion is the result of one chat turn.
type Completion struct {
	Message      Message
	FinishReason string
	TotalTokens  int
}

// Complete performs one chat completion, optionally offering tools.
func (c *Client) Complete(ctx context.Context, messages []Message, tools []Tool, temperature float64) (*Completion, error) {
	if c.APIKey == "" {
		return nil, fmt.Errorf("no OpenRouter API key is configured")
	}

	reqBody := chatRequest{
		Model:       c.Model,
		Messages:    messages,
		Tools:       tools,
		Temperature: temperature,
		MaxTokens:   4096,
	}
	if len(tools) > 0 {
		reqBody.ToolChoice = "auto"
	}

	buf, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	url := strings.TrimRight(c.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	if c.Referer != "" {
		req.Header.Set("HTTP-Referer", c.Referer)
	}
	if c.Title != "" {
		req.Header.Set("X-Title", c.Title)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call OpenRouter: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("OpenRouter rejected the API key (401)")
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("the free model is rate limited right now (429); try again shortly or pick another model")
	}

	var parsed chatResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("OpenRouter returned an unexpected response (HTTP %d): %s",
			resp.StatusCode, truncate(string(body), 300))
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, fmt.Errorf("OpenRouter error: %s", parsed.Error.Message)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("OpenRouter returned HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("the model returned no response")
	}

	return &Completion{
		Message:      parsed.Choices[0].Message,
		FinishReason: parsed.Choices[0].FinishReason,
		TotalTokens:  parsed.Usage.TotalTokens,
	}, nil
}

// ModelInfo is a model as listed by OpenRouter.
type ModelInfo struct {
	ID            string
	Name          string
	ContextLength int
	Free          bool
	ToolCapable   bool
}

// ListModels fetches the model catalogue. It works without an API key.
func (c *Client) ListModels(ctx context.Context) ([]ModelInfo, error) {
	url := strings.TrimRight(c.BaseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch model list: %w", err)
	}
	defer resp.Body.Close()

	var parsed struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			ContextLength int    `json:"context_length"`
			Pricing       struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
			SupportedParameters []string `json:"supported_parameters"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("parse model list: %w", err)
	}

	var out []ModelInfo
	for _, m := range parsed.Data {
		free := m.Pricing.Prompt == "0" && m.Pricing.Completion == "0"
		tools := false
		for _, p := range m.SupportedParameters {
			if p == "tools" {
				tools = true
				break
			}
		}
		out = append(out, ModelInfo{
			ID: m.ID, Name: m.Name, ContextLength: m.ContextLength,
			Free: free, ToolCapable: tools,
		})
	}
	return out, nil
}

// ListFreeToolModels returns only the free, tool-capable models, sorted for
// display. These are the models the agent can actually drive at no cost.
func (c *Client) ListFreeToolModels(ctx context.Context) ([]ModelInfo, error) {
	all, err := c.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	var out []ModelInfo
	for _, m := range all {
		if m.Free && m.ToolCapable {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
