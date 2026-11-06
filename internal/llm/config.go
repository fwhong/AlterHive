package llm

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// ModelInfo represents a model available from a provider.
type ModelInfo struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by,omitempty"`
}

// ProviderConfig represents a single LLM provider configuration.
type ProviderConfig struct {
	ID            string `yaml:"id" json:"id"`
	Name          string `yaml:"name" json:"name"`
	Type          string `yaml:"type" json:"type"` // "openai", "anthropic", "ollama"
	BaseURL       string `yaml:"base_url" json:"base_url"`
	KeyConfigured bool   `yaml:"-" json:"key_configured"`
	APIKey        string `yaml:"api_key" json:"api_key"`
	KeyEnv        string `yaml:"key_env,omitempty" json:"key_env,omitempty"`
	Model         string `yaml:"model" json:"model"`
	// Explicit opt-in for compatible providers; never infer vendor options from a model name.
	Thinking    string  `yaml:"thinking,omitempty" json:"thinking,omitempty"`
	MaxTokens   int     `yaml:"max_tokens" json:"max_tokens"`
	Temperature float64 `yaml:"temperature" json:"temperature"`
	Enabled     bool    `yaml:"enabled" json:"enabled"`
}

// APIKeyEnv returns the environment variable used for this provider's secret.
// A custom key_env wins; otherwise derive ALTERHIVE_<PROVIDER_ID>_API_KEY.
func (c ProviderConfig) APIKeyEnv() string {
	if env := strings.TrimSpace(c.KeyEnv); env != "" {
		return env
	}
	var name strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(c.ID)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			name.WriteRune(r)
		} else {
			name.WriteByte('_')
		}
	}
	return "ALTERHIVE_" + name.String() + "_API_KEY"
}

// ResolveAPIKey resolves a provider secret without persisting an environment
// value back to YAML. Environment configuration takes precedence over api_key.
func (c ProviderConfig) ResolveAPIKey() (string, string) {
	envName := c.APIKeyEnv()
	if value := strings.TrimSpace(os.Getenv(envName)); value != "" {
		return value, envName
	}
	return strings.TrimSpace(c.APIKey), "api_key"
}

// HasAPIKey reports whether a key is configured in either supported source.
func (c ProviderConfig) HasAPIKey() bool {
	key, _ := c.ResolveAPIKey()
	return key != ""
}

// LLMConfig is the top-level LLM configuration.
type LLMConfig struct {
	Enabled        bool             `yaml:"enabled" json:"enabled"`
	ActiveProvider string           `yaml:"active_provider" json:"active_provider"`
	Providers      []ProviderConfig `yaml:"providers" json:"providers"`
}

// LoadLLMConfig reads and parses the LLM config from a YAML file.
func LoadLLMConfig(path string) (*LLMConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var config LLMConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	// Reject unsafe endpoint configuration before any provider is instantiated.
	seen := make(map[string]bool)
	for i := range config.Providers {
		if err := validateProvider(config.Providers[i]); err != nil {
			return nil, fmt.Errorf("provider %d: %w", i, err)
		}
		if seen[config.Providers[i].ID] {
			return nil, fmt.Errorf("duplicate provider id")
		}
		seen[config.Providers[i].ID] = true
		if config.Providers[i].MaxTokens == 0 {
			config.Providers[i].MaxTokens = 2048
		}
		if config.Providers[i].Temperature == 0 {
			config.Providers[i].Temperature = 0.7
		}
	}
	if config.Enabled && config.GetActiveProvider() == nil {
		return nil, fmt.Errorf("enabled configuration requires an existing active provider")
	}
	return &config, nil
}

// GetProvider returns the provider config by ID, or nil if not found.
func (c *LLMConfig) GetProvider(id string) *ProviderConfig {
	for i := range c.Providers {
		if c.Providers[i].ID == id {
			return &c.Providers[i]
		}
	}
	return nil
}

// GetActiveProvider returns the currently active provider config.
func (c *LLMConfig) GetActiveProvider() *ProviderConfig {
	return c.GetProvider(c.ActiveProvider)
}

// Save writes the config back to the YAML file.
func (c *LLMConfig) Save(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".llm-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, path)
}

func validateProvider(c ProviderConfig) error {
	if c.ID == "" {
		return fmt.Errorf("provider id required")
	}
	if c.Type != "openai" && c.Type != "anthropic" && c.Type != "ollama" {
		return fmt.Errorf("unsupported provider type")
	}
	if c.Thinking != "" {
		if c.Type != "openai" || (c.Thinking != "enabled" && c.Thinking != "disabled") {
			return fmt.Errorf("thinking must be enabled or disabled for an openai-compatible provider")
		}
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("invalid provider base URL")
	}
	return nil
}
