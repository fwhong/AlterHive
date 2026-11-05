package engine

import (
	"context"
	"github.com/alterhive/alterhive/internal/deception"
	"github.com/alterhive/alterhive/internal/domain"
	"testing"
	"time"
)

type heldDecision struct {
	requests chan deception.DecisionRequest
	release  chan struct{}
}

func (d *heldDecision) IsActive() bool { return true }
func (d *heldDecision) Decide(ctx context.Context, req deception.DecisionRequest) (deception.DecisionResponse, error) {
	select {
	case d.requests <- req:
	case <-ctx.Done():
		return deception.DecisionResponse{}, ctx.Err()
	}
	select {
	case <-d.release:
		return deception.DecisionResponse{OK: true, Content: "synthetic breadcrumb\n"}, nil
	case <-ctx.Done():
		return deception.DecisionResponse{}, ctx.Err()
	}
}
func TestAsyncTerminalUsesOriginalCWDAndCoalesces(t *testing.T) {
	d := &heldDecision{make(chan deception.DecisionRequest, 2), make(chan struct{})}
	e := NewRuleEngine(testTopology(), domain.NewSafetyPolicy("192.168.56.0/24"), nil)
	e.SetDecider(d)
	s := domain.NewSessionContext("agent", "127.0.0.1:1")
	s.CWD = "/root"
	w := domain.NewWorldState()
	e.llmFallbackAsync("cat regression-clue", s, w)
	e.llmFallbackAsync("cat regression-clue", s, w)
	req := <-d.requests
	if req.Terminal.CWD != "/root" {
		t.Fatal("request CWD not frozen")
	}
	s.CWD = "/tmp"
	close(d.release)
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		s.ApplyPending()
		if w.Cache().Lookup(buildCacheKey("cat regression-clue", "/root")) != nil {
			break
		}
	}
	if w.GetFileEntry("/root/regression-clue") == nil || w.GetFileEntry("/tmp/regression-clue") != nil {
		t.Fatal("async result applied to wrong directory")
	}
	select {
	case <-d.requests:
		t.Fatal("duplicate in-flight terminal decision")
	default:
	}
}
func TestCachedFallbackKeepsLLMAttribution(t *testing.T) {
	e := NewRuleEngine(testTopology(), domain.NewSafetyPolicy("192.168.56.0/24"), nil)
	s := domain.NewSessionContext("agent", "127.0.0.1:1")
	w := domain.NewWorldState()
	storeAndInject("custom-status", "synthetic cached result", w, s.CWD)
	_, _, generated := e.HandleCommand("custom-status", s, w, false)
	if !generated {
		t.Fatal("cached LLM output incorrectly attributed to rules")
	}
}
