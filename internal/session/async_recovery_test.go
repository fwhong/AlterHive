package session

import (
	"context"
	"fmt"
	"github.com/alterhive/alterhive/internal/deception"
	"github.com/alterhive/alterhive/internal/domain"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type recoveringDecision struct{ active atomic.Bool }

func (d *recoveringDecision) IsActive() bool { return d.active.Load() }
func (d *recoveringDecision) Decide(_ context.Context, r deception.DecisionRequest) (deception.DecisionResponse, error) {
	if r.CallSite != deception.SiteComplexRender {
		return deception.DecisionResponse{}, fmt.Errorf("synthetic unavailable planner")
	}
	return deception.DecisionResponse{OK: true, Content: "Nmap scan report for 192.168.56.60\n3306/tcp open mysql MOCK-RECOVERY\n"}, nil
}
func TestSameCommandRecoversAfterInactiveManager(t *testing.T) {
	t.Setenv("ALTERHIVE_ALLOW_LLM_CONTEXT_EXPORT", "true")
	d := &recoveringDecision{}
	top := domain.NewVirtualTopology(domain.TopologyConfig{CIDR: "192.168.56.0/24", Hosts: []domain.VirtualHost{{IP: "192.168.56.60", Hostname: "db", Role: "database", Services: []domain.VirtualService{{Port: 3306, Protocol: "mysql", NmapName: "mysql"}}}}})
	m := NewManager(top, domain.NewSafetyPolicy("192.168.56.0/24"), noopTracer{}, "host", d)
	s := m.CreateSession("root", "127.0.0.1:1")
	s.Planning.ResponseCacheable = true
	command := "nmap -sV 192.168.56.60"
	if strings.Contains(m.ExecuteCommand(s.SessionID, command), "MOCK-RECOVERY") {
		t.Fatal("unexpected enriched first response")
	}
	d.active.Store(true)
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if strings.Contains(m.ExecuteCommand(s.SessionID, command), "MOCK-RECOVERY") {
			return
		}
	}
	t.Fatal("deterministic response cache masked recovered manager forever")
}
