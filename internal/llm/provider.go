package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Message represents a chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// CompletionRequest is the request to generate a completion.
type CompletionRequest struct {
	Messages       []Message `json:"messages"`
	MaxTokens      int       `json:"max_tokens,omitempty"`
	Temperature    float64   `json:"temperature,omitempty"`
	ResponseFormat string    `json:"response_format,omitempty"`
}

// CompletionResponse is the response from a completion.
type CompletionResponse struct {
	Content      string `json:"content"`
	Model        string `json:"model"`
	FinishReason string `json:"finish_reason,omitempty"`
	Usage        struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// Provider is the interface for LLM providers.
type Provider interface {
	// Name returns the provider display name.
	Name() string
	// Complete sends a completion request and returns the response.
	Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error)
	// TestConnection verifies the provider is reachable and credentials are valid.
	TestConnection(ctx context.Context) error
}

// NewProvider creates a Provider from config.
func NewProvider(cfg ProviderConfig) Provider {
	switch cfg.Type {
	case "anthropic":
		return newAnthropicProvider(cfg)
	case "ollama":
		return newOllamaProvider(cfg)
	default: // "openai" and compatible
		return newOpenAIProvider(cfg)
	}
}

// --- OpenAI-Compatible Provider ---

type openAIProvider struct {
	cfg    ProviderConfig
	client *http.Client
}

func newOpenAIProvider(cfg ProviderConfig) *openAIProvider {
	return &openAIProvider{
		cfg:    cfg,
		client: providerHTTPClient(120 * time.Second),
	}
}

func (p *openAIProvider) Name() string { return p.cfg.Name }

func (p *openAIProvider) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	url := strings.TrimRight(p.cfg.BaseURL, "/") + "/chat/completions"

	body := map[string]interface{}{
		"model":       p.cfg.Model,
		"messages":    req.Messages,
		"max_tokens":  req.MaxTokens,
		"temperature": req.Temperature,
	}
	if body["max_tokens"] == 0 {
		body["max_tokens"] = p.cfg.MaxTokens
	}
	if body["temperature"] == 0 {
		body["temperature"] = p.cfg.Temperature
	}
	if p.cfg.Thinking != "" {
		body["thinking"] = map[string]string{"type": p.cfg.Thinking}
	}
	if req.ResponseFormat == "json_object" {
		body["response_format"] = map[string]string{"type": "json_object"}
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error (%d)", resp.StatusCode)
	}

	var openAIResp struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Model string `json:"model"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &openAIResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if len(openAIResp.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}

	choice := openAIResp.Choices[0]
	// Truncated/refused/tool-call responses are not final terminal text or JSON.
	// Some compatible endpoints omit finish_reason; retain that legacy contract,
	// but never substitute hidden reasoning for a missing final answer.
	if choice.FinishReason != "" && choice.FinishReason != "stop" {
		return nil, fmt.Errorf("incomplete provider response")
	}
	content := choice.Message.Content
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("provider returned no final content")
	}
	result := &CompletionResponse{
		Content:      content,
		Model:        openAIResp.Model,
		FinishReason: choice.FinishReason,
	}
	result.Usage = openAIResp.Usage
	return result, nil
}

func (p *openAIProvider) TestConnection(ctx context.Context) error {
	base := strings.TrimRight(p.cfg.BaseURL, "/")

	// Try multiple URL patterns (same as fetchOpenAIModels)
	var candidates []string
	seen := make(map[string]bool)
	addCandidate := func(u string) {
		if !seen[u] {
			candidates = append(candidates, u)
			seen[u] = true
		}
	}

	if strings.Contains(base, "/chat/completions") {
		addCandidate(strings.Replace(base, "/chat/completions", "/models", 1))
	}
	if strings.HasSuffix(base, "/v1") {
		addCandidate(base + "/models")
	} else {
		addCandidate(base + "/v1/models")
		addCandidate(base + "/models")
	}

	for _, url := range candidates {
		httpReq, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			continue
		}
		if p.cfg.APIKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
		}

		resp, err := p.client.Do(httpReq)
		if err != nil {
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			return nil
		}
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("authentication failed (invalid API key)")
		}
	}

	return fmt.Errorf("no models endpoint reachable for %s", base)
}

// --- Anthropic Provider ---

type anthropicProvider struct {
	cfg    ProviderConfig
	client *http.Client
}

func newAnthropicProvider(cfg ProviderConfig) *anthropicProvider {
	return &anthropicProvider{
		cfg:    cfg,
		client: providerHTTPClient(120 * time.Second),
	}
}

func (p *anthropicProvider) Name() string { return p.cfg.Name }

func (p *anthropicProvider) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	url := strings.TrimRight(p.cfg.BaseURL, "/") + "/v1/messages"

	// Convert messages to Anthropic format
	var systemPrompt string
	var messages []map[string]string
	for _, msg := range req.Messages {
		if msg.Role == "system" {
			systemPrompt = msg.Content
			continue
		}
		messages = append(messages, map[string]string{
			"role":    msg.Role,
			"content": msg.Content,
		})
	}

	body := map[string]interface{}{
		"model":      p.cfg.Model,
		"messages":   messages,
		"max_tokens": req.MaxTokens,
	}
	if body["max_tokens"] == 0 {
		body["max_tokens"] = p.cfg.MaxTokens
	}
	if systemPrompt != "" {
		body["system"] = systemPrompt
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.cfg.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error (%d)", resp.StatusCode)
	}

	var anthropicResp struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Model string `json:"model"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &anthropicResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if anthropicResp.StopReason != "" && anthropicResp.StopReason != "end_turn" && anthropicResp.StopReason != "stop_sequence" {
		return nil, fmt.Errorf("incomplete provider response")
	}
	if len(anthropicResp.Content) == 0 {
		return nil, fmt.Errorf("no content in response")
	}

	var textParts []string
	for _, c := range anthropicResp.Content {
		if c.Type == "text" {
			textParts = append(textParts, c.Text)
		}
	}

	if strings.TrimSpace(strings.Join(textParts, "")) == "" {
		return nil, fmt.Errorf("provider returned no final content")
	}
	result := &CompletionResponse{
		FinishReason: anthropicResp.StopReason,
		Content:      strings.Join(textParts, ""),
		Model:        anthropicResp.Model,
	}
	result.Usage.PromptTokens = anthropicResp.Usage.InputTokens
	result.Usage.CompletionTokens = anthropicResp.Usage.OutputTokens
	result.Usage.TotalTokens = anthropicResp.Usage.InputTokens + anthropicResp.Usage.OutputTokens
	return result, nil
}

func (p *anthropicProvider) TestConnection(ctx context.Context) error {
	url := strings.TrimRight(p.cfg.BaseURL, "/") + "/v1/messages"

	body := map[string]interface{}{
		"model":      p.cfg.Model,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	}
	jsonBody, _ := json.Marshal(body)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.cfg.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("authentication failed (invalid API key)")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("API error (%d)", resp.StatusCode)
	}
	return nil
}

// --- Ollama Provider (native /api/chat) ---

type ollamaProvider struct {
	cfg    ProviderConfig
	client *http.Client
}

func newOllamaProvider(cfg ProviderConfig) *ollamaProvider {
	return &ollamaProvider{
		cfg:    cfg,
		client: providerHTTPClient(120 * time.Second),
	}
}

func (p *ollamaProvider) Name() string { return p.cfg.Name }

func (p *ollamaProvider) ollamaBase() string {
	base := strings.TrimRight(p.cfg.BaseURL, "/")
	base = strings.TrimSuffix(base, "/v1")
	if base == "" {
		base = "http://127.0.0.1:11434"
	}
	return base
}

func (p *ollamaProvider) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	url := p.ollamaBase() + "/api/chat"

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = p.cfg.MaxTokens
	}

	var messages []map[string]string
	for _, msg := range req.Messages {
		messages = append(messages, map[string]string{
			"role":    msg.Role,
			"content": msg.Content,
		})
	}

	body := map[string]interface{}{
		"model":    p.cfg.Model,
		"messages": messages,
		"stream":   false,
		"options": map[string]interface{}{
			"num_predict": maxTokens,
		},
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Ollama API error (%d)", resp.StatusCode)
	}

	var ollamaResp struct {
		Done       *bool  `json:"done"`
		DoneReason string `json:"done_reason"`
		Message    struct {
			Content string `json:"content"`
		} `json:"message"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(respBody, &ollamaResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if (ollamaResp.Done != nil && !*ollamaResp.Done) || (ollamaResp.DoneReason != "" && ollamaResp.DoneReason != "stop") {
		return nil, fmt.Errorf("incomplete provider response")
	}
	if strings.TrimSpace(ollamaResp.Message.Content) == "" {
		return nil, fmt.Errorf("provider returned no final content")
	}
	return &CompletionResponse{
		FinishReason: ollamaResp.DoneReason,
		Content:      ollamaResp.Message.Content,
		Model:        ollamaResp.Model,
	}, nil
}

func (p *ollamaProvider) TestConnection(ctx context.Context) error {
	url := p.ollamaBase() + "/api/tags"

	httpReq, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Ollama API error (%d)", resp.StatusCode)
	}
	return nil
}

func providerHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
