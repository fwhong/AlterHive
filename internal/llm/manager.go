package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// Manager manages LLM providers and handles request routing.
type Manager struct {
	mu         sync.RWMutex
	config     *LLMConfig
	configPath string
	readOnly   bool
	providers  map[string]Provider
	active     Provider
}

// NewManager creates a new LLM manager from config.
func NewManager(config *LLMConfig, configPath string) *Manager {
	m := &Manager{
		config:     cloneConfig(config),
		configPath: configPath,
		providers:  make(map[string]Provider),
	}

	// Initialize all enabled providers
	for _, cfg := range m.config.Providers {
		if cfg.Enabled && (cfg.HasAPIKey() || cfg.Type == "ollama") {
			live := cfg
			if key, _ := cfg.ResolveAPIKey(); key != "" {
				live.APIKey = key
			}
			p := NewProvider(live)
			m.providers[cfg.ID] = p
			log.WithField("provider", cfg.ID).Info("LLM provider initialized")
		}
	}

	// Set active provider
	if active := m.config.GetActiveProvider(); active != nil && active.Enabled {
		if p, ok := m.providers[active.ID]; ok {
			m.active = p
			log.WithField("provider", active.ID).Info("Active LLM provider set")
		}
	}

	return m
}

// IsActive returns whether an LLM provider is active and ready.
func (m *Manager) IsActive() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config.Enabled && m.active != nil
}

// GetActiveProvider returns the active provider config.
func (m *Manager) GetActiveProvider() *ProviderConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.active == nil {
		return nil
	}
	cfg := m.config.GetActiveProvider()
	if cfg == nil {
		return nil
	}
	out := *cfg
	out.KeyConfigured = out.HasAPIKey()
	out.APIKey = ""
	return &out
}

// Complete sends a completion request to the active provider.
func (m *Manager) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	if !allowProviderNetworkRequests() {
		return nil, fmt.Errorf("LLM provider network requests are disabled")
	}

	m.mu.RLock()
	p := m.active
	enabled := m.config.Enabled
	// Capture the exact in-flight credential before concurrent configuration rotation.
	var keys []string
	for _, cfg := range m.config.Providers {
		key, _ := cfg.ResolveAPIKey()
		if key != "" {
			keys = append(keys, key)
		}
	}
	m.mu.RUnlock()

	if p == nil || !enabled {
		return nil, fmt.Errorf("no active LLM provider configured")
	}
	resp, err := p.Complete(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("provider completion failed")
	}
	if resp == nil {
		return nil, fmt.Errorf("empty provider response")
	}
	for _, key := range keys {
		resp.Content = strings.ReplaceAll(resp.Content, key, "[REDACTED]")
	}
	return resp, nil
}

func allowProviderNetworkRequests() bool {
	return strings.EqualFold(os.Getenv("ALTERHIVE_ALLOW_LLM_NETWORK"), "true")
}

// TestConnection never mutates configuration or reuses a key for another URL.
func (m *Manager) TestConnection(ctx context.Context, providerID, baseURL, key string) error {
	cfg, err := m.connectionConfig(providerID, baseURL, key)
	if err != nil {
		return err
	}
	return NewProvider(cfg).TestConnection(ctx)
}

func (m *Manager) connectionConfig(id, baseURL, key string) (ProviderConfig, error) {
	if !allowProviderNetworkRequests() {
		return ProviderConfig{}, fmt.Errorf("LLM provider network requests are disabled")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	p := m.config.GetProvider(id)
	if p == nil {
		return ProviderConfig{}, fmt.Errorf("provider not found")
	}
	cfg := *p
	// Even a changed path could expose credentials; overrides must exactly match.
	if baseURL != "" && strings.TrimRight(baseURL, "/") != strings.TrimRight(cfg.BaseURL, "/") {
		return ProviderConfig{}, fmt.Errorf("provider URL override is not permitted")
	}
	if key != "" {
		return ProviderConfig{}, fmt.Errorf("credential overrides are not permitted")
	}
	cfg.APIKey, _ = cfg.ResolveAPIKey()
	if err := validateProvider(cfg); err != nil {
		return ProviderConfig{}, err
	}
	return cfg, nil
}

// commitLocked persists BEFORE changing live state. Failure leaves everything intact.
func (m *Manager) commitLocked(next *LLMConfig) error {
	if m.readOnly {
		return fmt.Errorf("LLM configuration is read-only")
	}
	if m.configPath != "" {
		if err := next.Save(m.configPath); err != nil {
			return fmt.Errorf("cannot persist LLM configuration: %w", err)
		}
	}
	m.config = next
	m.rebuildLocked()
	return nil
}
func (m *Manager) rebuildLocked() {
	m.providers = make(map[string]Provider)
	m.active = nil
	for _, cfg := range m.config.Providers {
		if cfg.Enabled && (cfg.HasAPIKey() || cfg.Type == "ollama") {
			live := cfg
			live.APIKey, _ = cfg.ResolveAPIKey()
			m.providers[cfg.ID] = NewProvider(live)
		}
	}
	if m.config.Enabled {
		m.active = m.providers[m.config.ActiveProvider]
	}
}
func cloneConfig(c *LLMConfig) *LLMConfig {
	n := *c
	n.Providers = append([]ProviderConfig{}, c.Providers...)
	return &n
}
func (m *Manager) SetReadOnly(v bool) { m.mu.Lock(); defer m.mu.Unlock(); m.readOnly = v }
func (m *Manager) ReadOnly() bool     { m.mu.RLock(); defer m.mu.RUnlock(); return m.readOnly }
func (m *Manager) SwitchProvider(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := cloneConfig(m.config)
	cfg := next.GetProvider(id)
	if cfg == nil || !cfg.Enabled || (!cfg.HasAPIKey() && cfg.Type != "ollama") {
		return fmt.Errorf("provider unavailable")
	}
	next.ActiveProvider = id
	return m.commitLocked(next)
}
func (m *Manager) SetEnabled(enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := cloneConfig(m.config)
	next.Enabled = enabled
	return m.commitLocked(next)
}
func (m *Manager) UpdateProvider(cfg ProviderConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := cloneConfig(m.config)
	if old := next.GetProvider(cfg.ID); old != nil {
		if cfg.BaseURL != old.BaseURL || cfg.KeyEnv != old.KeyEnv {
			// Binding changes cannot silently carry a saved/environment key to a new origin.
			if old.HasAPIKey() {
				return fmt.Errorf("credential-bound provider endpoint cannot be changed; create a new provider")
			}
		}
		if cfg.APIKey == "" || strings.Contains(cfg.APIKey, "****") {
			cfg.APIKey = old.APIKey
		}
		*old = cfg
	} else {
		next.Providers = append(next.Providers, cfg)
	}
	if err := validateProvider(cfg); err != nil {
		return err
	}
	return m.commitLocked(next)
}

// Public snapshots contain presence flags, NEVER keys or caller-mutable backing slices.
func (m *Manager) GetConfig() LLMConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cfg := cloneConfig(m.config)
	for i := range cfg.Providers {
		cfg.Providers[i].KeyConfigured = cfg.Providers[i].HasAPIKey()
		cfg.Providers[i].APIKey = ""
	}
	return *cfg
}
func (m *Manager) ListProviders() []ProviderConfig { return m.GetConfig().Providers }

// FetchModels fetches the available model list from a provider's API.
// Overrides cannot redirect a saved credential; only the saved endpoint is used.
func (m *Manager) FetchModels(ctx context.Context, providerID, overrideBaseURL, overrideAPIKey string) ([]ModelInfo, error) {
	if !allowProviderNetworkRequests() {
		return nil, fmt.Errorf("LLM provider network requests are disabled")
	}

	cfg, err := m.connectionConfig(providerID, overrideBaseURL, overrideAPIKey)
	if err != nil {
		return nil, err
	}
	baseURL, apiKey := cfg.BaseURL, cfg.APIKey

	switch cfg.Type {
	case "anthropic":
		return fetchAnthropicModels()
	case "ollama":
		return fetchOllamaModels(ctx, baseURL)
	default:
		return fetchOpenAIModels(ctx, baseURL, apiKey)
	}
}

// fetchOllamaModels fetches models from Ollama's /api/tags endpoint (no API key needed).
func fetchOllamaModels(ctx context.Context, baseURL string) ([]ModelInfo, error) {
	base := strings.TrimRight(baseURL, "/")
	// Strip /v1 suffix if present — Ollama API is at root
	base = strings.TrimSuffix(base, "/v1")
	if base == "" {
		base = "http://127.0.0.1:11434"
	}

	url := base + "/api/tags"
	httpReq, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	client := providerHTTPClient(15 * time.Second)
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Ollama API error (%d)", resp.StatusCode)
	}

	var result struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	models := make([]ModelInfo, 0, len(result.Models))
	for _, m := range result.Models {
		if m.Name != "" {
			models = append(models, ModelInfo{ID: m.Name, OwnedBy: "ollama"})
		}
	}
	return models, nil
}

// fetchOpenAIModels fetches models from an OpenAI-compatible /v1/models endpoint.
// Tries multiple URL patterns to handle different provider configurations.
func fetchOpenAIModels(ctx context.Context, baseURL, apiKey string) ([]ModelInfo, error) {
	base := strings.TrimRight(baseURL, "/")

	// Build candidate URLs (mirrors V1.0.0 logic)
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

	client := providerHTTPClient(15 * time.Second)

	for _, url := range candidates {
		httpReq, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			continue
		}
		if apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+apiKey)
		}

		resp, err := client.Do(httpReq)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			continue
		}

		// Try standard OpenAI format: {"data": [{"id": "...", "owned_by": "..."}]}
		var openAIResult struct {
			Data []struct {
				ID      string `json:"id"`
				OwnedBy string `json:"owned_by"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &openAIResult); err == nil && len(openAIResult.Data) > 0 {
			models := make([]ModelInfo, 0, len(openAIResult.Data))
			for _, m := range openAIResult.Data {
				models = append(models, ModelInfo{ID: m.ID, OwnedBy: m.OwnedBy})
			}
			return models, nil
		}

		// Try plain array format: [{"id": "..."}]
		var plainResult []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		}
		if err := json.Unmarshal(body, &plainResult); err == nil && len(plainResult) > 0 {
			models := make([]ModelInfo, 0, len(plainResult))
			for _, m := range plainResult {
				models = append(models, ModelInfo{ID: m.ID, OwnedBy: m.OwnedBy})
			}
			return models, nil
		}
	}

	return nil, fmt.Errorf("no models endpoint reachable for %s", baseURL)
}

func fetchAnthropicModels() ([]ModelInfo, error) {
	// Anthropic has no public models API — return known models
	return []ModelInfo{
		{ID: "claude-opus-4-20250514", OwnedBy: "anthropic"},
		{ID: "claude-sonnet-4-20250514", OwnedBy: "anthropic"},
		{ID: "claude-haiku-4-5-20251001", OwnedBy: "anthropic"},
		{ID: "claude-3-5-sonnet-20241022", OwnedBy: "anthropic"},
		{ID: "claude-3-5-haiku-20241022", OwnedBy: "anthropic"},
		{ID: "claude-3-opus-20240229", OwnedBy: "anthropic"},
	}, nil
}
