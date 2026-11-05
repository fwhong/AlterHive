package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func testManager(t *testing.T, url, path string) *Manager {
	t.Helper()
	t.Setenv("ALTERHIVE_ALLOW_LLM_NETWORK", "true")
	return NewManager(&LLMConfig{Enabled: true, ActiveProvider: "mock", Providers: []ProviderConfig{{ID: "mock", Type: "openai", BaseURL: url, APIKey: "SYNTHETIC-YAML-SECRET", Model: "mock", Enabled: true}}}, path)
}
func TestPublicConfigurationNeverContainsKeys(t *testing.T) {
	t.Setenv("ALTERHIVE_MOCK_API_KEY", "SYNTHETIC-ENV-SECRET")
	m := testManager(t, "http://127.0.0.1/v1", "")
	for _, v := range []any{m.GetConfig(), m.ListProviders(), m.GetActiveProvider()} {
		b, _ := json.Marshal(v)
		if strings.Contains(string(b), "SECRET") {
			t.Fatalf("secret disclosed: %s", b)
		}
	}
	cfg := m.GetConfig()
	if !cfg.Providers[0].KeyConfigured {
		t.Fatal("missing presence flag")
	}
	cfg.Providers[0].BaseURL = "changed"
	if m.GetConfig().Providers[0].BaseURL == "changed" {
		t.Fatal("mutable public slice")
	}
}
func TestOverridesAndRedirectsCannotForwardKeys(t *testing.T) {
	var received atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.Write([]byte(`{"data":[{"id":"mock"}]}`))
	}))
	defer sink.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, sink.URL+r.URL.Path, 302) }))
	defer source.Close()
	m := testManager(t, source.URL+"/v1", "")
	if _, e := m.FetchModels(context.Background(), "mock", sink.URL+"/v1", ""); e == nil {
		t.Fatal("accepted changed URL")
	}
	if e := m.TestConnection(context.Background(), "mock", sink.URL+"/v1", ""); e == nil {
		t.Fatal("accepted changed test URL")
	}
	m.FetchModels(context.Background(), "mock", "", "")
	m.TestConnection(context.Background(), "mock", "", "")
	m.Complete(context.Background(), CompletionRequest{MaxTokens: 1})
	if received.Load() != 0 {
		t.Fatal("key forwarded across redirect")
	}
	cfg := m.GetConfig().Providers[0]
	cfg.BaseURL = sink.URL
	if m.UpdateProvider(cfg) == nil {
		t.Fatal("rebound saved credential")
	}
}
func TestTestConnectionDoesNotPersistEnvironmentKey(t *testing.T) {
	t.Setenv("ALTERHIVE_MOCK_API_KEY", "SYNTHETIC-ENV-SECRET")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"data":[{"id":"mock"}]}`)) }))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "llm.yaml")
	m := testManager(t, server.URL+"/v1", path)
	if e := m.TestConnection(context.Background(), "mock", "", ""); e != nil {
		t.Fatal(e)
	}
	if e := m.SetEnabled(false); e != nil {
		t.Fatal(e)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "SYNTHETIC-ENV-SECRET") {
		t.Fatal("env key persisted")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("secret file permissions")
	}
}
func TestConfigurationFailureIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "llm.yaml")
	m := testManager(t, "http://127.0.0.1/v1", path)
	before, _ := json.Marshal(m.GetConfig())
	if m.SetEnabled(false) == nil {
		t.Fatal("expected write failure")
	}
	if !m.IsActive() {
		t.Fatal("failed save changed active state")
	}
	cfg := m.GetConfig().Providers[0]
	cfg.Model = "changed"
	if m.UpdateProvider(cfg) == nil {
		t.Fatal("expected provider save failure")
	}
	after, _ := json.Marshal(m.GetConfig())
	if string(before) != string(after) {
		t.Fatal("failed transaction mutated config")
	}
	m.SetReadOnly(true)
	if m.SetEnabled(false) == nil || !m.IsActive() {
		t.Fatal("read-only mutation")
	}
}
func TestProviderErrorAndContentDoNotLeakKey(t *testing.T) {
	var errorMode atomic.Bool
	errorMode.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if errorMode.Load() {
			w.WriteHeader(500)
			w.Write([]byte(r.Header.Get("Authorization")))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": r.Header.Get("Authorization")}}}})
	}))
	defer server.Close()
	m := testManager(t, server.URL+"/v1", "")
	_, err := m.Complete(context.Background(), CompletionRequest{})
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("upstream error leaks key")
	}
	errorMode.Store(false)
	resp, err := m.Complete(context.Background(), CompletionRequest{})
	if err != nil || strings.Contains(resp.Content, "SECRET") {
		t.Fatalf("content leaks key: %#v %v", resp, err)
	}
}

func TestLoadRejectsUnsafeEndpoint(t *testing.T) {
	for _, url := range []string{"https://key@example.test/v1", "https://example.test/v1?key=secret", "file:///tmp/provider"} {
		cfg := &LLMConfig{Providers: []ProviderConfig{{ID: "mock", Type: "openai", BaseURL: url}}}
		path := filepath.Join(t.TempDir(), "llm.yaml")
		if err := cfg.Save(path); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadLLMConfig(path); err == nil {
			t.Fatal("unsafe endpoint accepted")
		}
	}
}
func TestDisabledManagerCannotComplete(t *testing.T) {
	m := testManager(t, "http://127.0.0.1/v1", "")
	if err := m.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Complete(context.Background(), CompletionRequest{}); err == nil {
		t.Fatal("disabled manager called provider")
	}
}
