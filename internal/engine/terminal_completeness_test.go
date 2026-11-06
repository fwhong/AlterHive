package engine

import (
	"context"
	"github.com/alterhive/alterhive/internal/deception"
	"github.com/alterhive/alterhive/internal/domain"
	"strings"
	"testing"
	"time"
)

const completeScan = "Nmap scan report for 192.168.56.60\nPORT STATE SERVICE\n22/tcp open ssh\n3306/tcp open mysql\n"

func TestScanEnrichmentPreservesRequiredFacts(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		valid        bool
	}{
		{"real_partial", "Starting Nmap 7\n", false},
		{"missing_port", "Nmap scan report for 192.168.56.60\n22/tcp open ssh\n", false},
		{"missing_target", "22/tcp open ssh\n3306/tcp open mysql\n", false},
		{"substring_port", "Nmap scan report for 192.168.56.60\n122/tcp open ssh\n3306/tcp open mysql\n", false},
		{"full", completeScan, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if preservesScanFacts("nmap", completeScan, tc.output) != tc.valid {
				t.Fatal(tc.output)
			}
		})
	}
}

type scanDecision struct{ replies chan string }

func (*scanDecision) IsActive() bool { return true }
func (d *scanDecision) Decide(ctx context.Context, _ deception.DecisionRequest) (deception.DecisionResponse, error) {
	select {
	case s := <-d.replies:
		return deception.DecisionResponse{OK: true, Content: s}, nil
	case <-ctx.Done():
		return deception.DecisionResponse{}, ctx.Err()
	}
}
func TestPartialScanCannotPoisonCacheAndCanRecover(t *testing.T) {
	t.Setenv("ALTERHIVE_ALLOW_LLM_CONTEXT_EXPORT", "true")
	d := &scanDecision{make(chan string, 2)}
	e := NewRuleEngine(testTopology(), domain.NewSafetyPolicy("192.168.56.0/24"), nil)
	e.SetDecider(d)
	s := domain.NewSessionContext("agent", "127.0.0.1:1")
	w := domain.NewWorldState()
	cmd := "nmap 192.168.56.60"
	d.replies <- "Starting Nmap 7\n"
	e.handleComplexAsync(cmd, "nmap", s, w, completeScan)
	wait := func(ready func() bool) {
		t.Helper()
		for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			s.ApplyPending()
			if ready() {
				return
			}
		}
		t.Fatal("mutation not applied")
	}
	wait(func() bool {
		for _, event := range s.Events() {
			if strings.Contains(event.Detail, "complex_missing_scan_facts") {
				return true
			}
		}
		return false
	})
	if w.Cache().Lookup(buildCacheKey(cmd, s.CWD)) != nil {
		t.Fatal("partial output cached")
	}
	d.replies <- completeScan
	e.handleComplexAsync(cmd, "nmap", s, w, completeScan)
	wait(func() bool { return w.Cache().Lookup(buildCacheKey(cmd, s.CWD)) != nil })
	output, _, generated := e.HandleCommand(cmd, s, w, false)
	if !generated || !strings.Contains(output, "3306/tcp") {
		t.Fatalf("valid retry not applied: %q", output)
	}
}
