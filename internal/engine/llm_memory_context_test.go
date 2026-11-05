package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alterhive/alterhive/internal/domain"
	"github.com/alterhive/alterhive/internal/llm"
)

// capturingLLMClient records the system prompt sent to the LLM so tests can
// assert that the L0/L1/L2 layered memory context was injected.
type capturingLLMClient struct {
	mu           sync.Mutex
	active       bool
	content      string
	systemPrompt string
}

func (f *capturingLLMClient) IsActive() bool { return f.active }

func (f *capturingLLMClient) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.systemPrompt = ""
	for _, msg := range req.Messages {
		if msg.Role == "system" {
			f.systemPrompt += msg.Content + "\n"
		}
	}
	return &llm.CompletionResponse{Content: f.content}, nil
}

// awaitSystemPrompt polls until the async decision call reaches the
// capturing completion client with a system prompt.
func awaitSystemPrompt(t *testing.T, client *capturingLLMClient) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		client.mu.Lock()
		prompt := client.systemPrompt
		client.mu.Unlock()
		if prompt != "" {
			return prompt
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the async decision system prompt")
	return ""
}

// TestLLMFallbackInjectsMemoryContext verifies that the LLM system prompt
// includes the L2 invariants (host identity, network entry) from the Memory
// Manager, per architecture §3.2 (L0/L1/L2 layered context).
func TestLLMFallbackInjectsMemoryContext(t *testing.T) {
	topology := testTopology()
	engine := NewRuleEngine(topology, domain.NewSafetyPolicy("192.168.56.0/24"), domain.NewServiceRegistry(topology))
	client := &capturingLLMClient{active: true, content: "deploy:x:1001:1001::/home/deploy:/bin/bash\n"}
	engine.SetLLMClient(client)

	session := domain.NewSessionContext("www-data", "127.0.0.1:0")
	session.Memory.SetInvariant("host.identity", "staging-web-01|Ubuntu 22.04|192.168.56.23", "test")
	session.Memory.SetInvariant("network.entry", "dual-nic entry: eth0 dynamic, eth1 192.168.56.23/24", "test")
	t.Setenv("ALTERHIVE_ALLOW_LLM_CONTEXT_EXPORT", "true")
	world := domain.NewWorldState()

	// stat of an unknown file path triggers LLM fallback (not in world state, not a protected file)
	engine.HandleCommand("stat /opt/webapp/deploy.trace", session, world, false)

	systemPrompt := awaitSystemPrompt(t, client)
	// L2 invariant: host identity must be in the prompt
	if !strings.Contains(systemPrompt, "host.identity") {
		t.Fatalf("expected L2 host.identity invariant in system prompt, got: %q", systemPrompt)
	}
	// L2 invariant: network entry must be in the prompt
	if !strings.Contains(systemPrompt, "network.entry") {
		t.Fatalf("expected L2 network.entry invariant in system prompt, got: %q", systemPrompt)
	}
}

// TestLLMFallbackInjectsContextAwareSlice verifies that context-aware L1 facts
// are selectively injected based on command semantics (architecture §3.2
// buildL1Context). A secret-hunting cat command should include the secret
// hunting hint.
func TestLLMFallbackInjectsContextAwareSlice(t *testing.T) {
	topology := testTopology()
	engine := NewRuleEngine(topology, domain.NewSafetyPolicy("192.168.56.0/24"), domain.NewServiceRegistry(topology))
	client := &capturingLLMClient{active: true, content: "DB_PASSWORD=Summer2023_backup!\n"}
	engine.SetLLMClient(client)

	session := domain.NewSessionContext("www-data", "127.0.0.1:0")
	// Simulate that the memory has observed secret-hunting behavior
	session.Memory.ObserveCommand("grep password /opt/webapp/.env", "", []string{"pseudo_progress"})
	t.Setenv("ALTERHIVE_ALLOW_LLM_CONTEXT_EXPORT", "true")
	world := domain.NewWorldState()

	// cat of an unknown config file triggers LLM fallback and matches the
	// cat/grep context-aware slice
	engine.HandleCommand("cat /opt/webapp/secrets.env", session, world, false)

	systemPrompt := awaitSystemPrompt(t, client)
	// The context-aware slice should mention secret hunting
	if !strings.Contains(systemPrompt, "secret") {
		t.Fatalf("expected secret-hunting context in prompt for cat command, got: %q", systemPrompt)
	}
}
