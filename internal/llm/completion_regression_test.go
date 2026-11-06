package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIncompleteCompletionsNeverBecomeFinalOutput(t *testing.T) {
	cases := []struct {
		name, kind, body string
		wantError        bool
	}{
		{"openai_length", "openai", `{"choices":[{"finish_reason":"length","message":{"content":"Starting Nmap 7","reasoning_content":"PRIVATE ANALYSIS"}}]}`, true},
		{"openai_filter", "openai", `{"choices":[{"finish_reason":"content_filter","message":{"content":"partial"}}]}`, true},
		{"openai_tools", "openai", `{"choices":[{"finish_reason":"tool_calls","message":{"content":"partial"}}]}`, true},
		{"openai_reasoning_only", "openai", `{"choices":[{"finish_reason":"stop","message":{"content":"","reasoning_content":"PRIVATE ANALYSIS"}}]}`, true},
		{"openai_whitespace", "openai", `{"choices":[{"message":{"content":"  ","reasoning_content":"PRIVATE ANALYSIS"}}]}`, true},
		{"openai_final", "openai", `{"choices":[{"finish_reason":"stop","message":{"content":"final","reasoning_content":"PRIVATE ANALYSIS"}}]}`, false},
		{"openai_legacy", "openai", `{"choices":[{"message":{"content":"final"}}]}`, false},
		{"anthropic_length", "anthropic", `{"stop_reason":"max_tokens","content":[{"type":"text","text":"partial"}]}`, true},
		{"anthropic_tool", "anthropic", `{"stop_reason":"tool_use","content":[{"type":"text","text":"partial"}]}`, true},
		{"anthropic_reasoning", "anthropic", `{"stop_reason":"end_turn","content":[{"type":"thinking","text":"PRIVATE ANALYSIS"}]}`, true},
		{"anthropic_final", "anthropic", `{"stop_reason":"end_turn","content":[{"type":"text","text":"final"}]}`, false},
		{"anthropic_sequence", "anthropic", `{"stop_reason":"stop_sequence","content":[{"type":"text","text":"final"}]}`, false},
		{"ollama_length", "ollama", `{"done":true,"done_reason":"length","message":{"content":"partial"}}`, true},
		{"ollama_unfinished", "ollama", `{"done":false,"message":{"content":"partial"}}`, true},
		{"ollama_empty", "ollama", `{"done":true,"done_reason":"stop","message":{"content":""}}`, true},
		{"ollama_final", "ollama", `{"done":true,"done_reason":"stop","message":{"content":"final"}}`, false},
		{"ollama_legacy", "ollama", `{"message":{"content":"final"}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(tc.body))
			}))
			defer server.Close()
			p := NewProvider(ProviderConfig{Type: tc.kind, BaseURL: server.URL, Model: "test", MaxTokens: 800})
			got, err := p.Complete(context.Background(), CompletionRequest{})
			if (err != nil) != tc.wantError {
				t.Fatalf("want error %v; got %#v, %v", tc.wantError, got, err)
			}
			if err != nil && strings.Contains(err.Error(), "PRIVATE ANALYSIS") {
				t.Fatal("reasoning leaked in error")
			}
			if !tc.wantError && got.Content != "final" {
				t.Fatalf("not final content: %#v", got)
			}
		})
	}
}

func TestThinkingIsExplicitAndPreservesJSONContract(t *testing.T) {
	for _, thinking := range []string{"", "disabled", "enabled"} {
		t.Run("thinking_"+thinking, func(t *testing.T) {
			received := make(chan map[string]any, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var b map[string]any
				json.NewDecoder(r.Body).Decode(&b)
				received <- b
				w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"{}"}}]}`))
			}))
			defer server.Close()
			p := NewProvider(ProviderConfig{Type: "openai", BaseURL: server.URL, Model: "test", Thinking: thinking})
			_, err := p.Complete(context.Background(), CompletionRequest{MaxTokens: 800, ResponseFormat: "json_object"})
			if err != nil {
				t.Fatal(err)
			}
			body := <-received
			if thinking == "" {
				if _, ok := body["thinking"]; ok {
					t.Fatal("vendor option injected by default")
				}
			} else if body["thinking"].(map[string]any)["type"] != thinking {
				t.Fatal("thinking option missing")
			}
			if body["max_tokens"] != float64(800) || body["response_format"].(map[string]any)["type"] != "json_object" {
				t.Fatal("decision parameters overridden")
			}
		})
	}
}

func TestThinkingConfigurationValidationAndPersistence(t *testing.T) {
	for _, tc := range []struct {
		kind, thinking string
		valid          bool
	}{{"openai", "", true}, {"openai", "enabled", true}, {"openai", "disabled", true}, {"openai", "false", false}, {"ollama", "disabled", false}, {"anthropic", "disabled", false}} {
		t.Run(tc.kind+"_"+tc.thinking, func(t *testing.T) {
			cfg := ProviderConfig{ID: "test", Type: tc.kind, BaseURL: "https://example.invalid/v1", Thinking: tc.thinking, Model: "model"}
			if (validateProvider(cfg) == nil) != tc.valid {
				t.Fatal("unexpected validation result")
			}
			if !tc.valid {
				return
			}
			file := filepath.Join(t.TempDir(), "llm.yaml")
			c := &LLMConfig{Providers: []ProviderConfig{cfg}}
			if err := c.Save(file); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadLLMConfig(file)
			if err != nil || loaded.Providers[0].Thinking != tc.thinking {
				t.Fatal("thinking option lost", err)
			}
			m := NewManager(loaded, file)
			if m.GetConfig().Providers[0].Thinking != tc.thinking {
				t.Fatal("snapshot lost option")
			}
			st, _ := os.Stat(file)
			if st.Mode().Perm() != 0600 {
				t.Fatal("configuration permissions changed")
			}
		})
	}
}
