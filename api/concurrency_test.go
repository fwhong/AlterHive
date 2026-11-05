package api

import (
	"fmt"
	"sync"
	"testing"

	"github.com/alterhive/alterhive/internal/domain"
	"github.com/alterhive/alterhive/internal/session"
	"github.com/alterhive/alterhive/internal/tracer"
	"github.com/gin-gonic/gin"
)

type silentTracer struct{}

func (silentTracer) TraceEvent(tracer.Event) {}
func TestConcurrentCommandsAndManagementSnapshots(t *testing.T) {
	t.Setenv("ALTERHIVE_ALLOW_LLM_CONTEXT_EXPORT", "false")
	top := domain.NewVirtualTopology(domain.TopologyConfig{CIDR: "192.168.56.0/24", LocalIP: "192.168.56.23"})
	m := session.NewManager(top, domain.NewSafetyPolicy("192.168.56.0/24"), silentTracer{}, "host", nil)
	s := m.CreateSession("root", "127.0.0.1:2222")
	r := gin.New()
	NewLocalServer(m).RegisterRoutes(r)
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				switch worker {
				case 0:
					m.ExecuteNonInteractiveCommand(s.SessionID, "pwd")
				case 1:
					m.SetEntryNetwork(s.SessionID, "192.168.56.23", "192.168.56.0/24", "192.168.56.1")
					m.ExecuteCommand(s.SessionID, "cd /tmp")
					m.MarkDisconnected(s.SessionID)
				case 2:
					for _, path := range []string{"/api/v1/sessions", "/api/v1/dashboard/summary", "/api/v1/commands", "/api/v1/sessions/" + s.SessionID, "/api/v1/sessions/" + s.SessionID + "/topology"} {
						if w := call(r, "GET", path, "", ""); w.Code != 200 {
							t.Errorf("%s: %d", path, w.Code)
						}
					}
				case 3:
					if w := call(r, "POST", "/api/v1/simulate/ping", fmt.Sprintf(`{"target":"192.168.56.23","session_id":%q}`, s.SessionID), ""); w.Code != 200 {
						t.Error(w.Code)
					}
				}
			}
		}(worker)
	}
	wg.Wait()
}

func TestSimulationReusesLiveSessionForGoalMutations(t *testing.T) {
	top := domain.NewVirtualTopology(domain.TopologyConfig{CIDR: "192.168.56.0/24", LocalIP: "192.168.56.23"})
	m := session.NewManager(top, domain.NewSafetyPolicy("192.168.56.0/24"), silentTracer{}, "host", nil)
	live := m.CreateSession("root", "127.0.0.1:1")
	server := NewLocalServer(m)
	if server.getOrCreateSimSession(live.SessionID) != live {
		t.Fatal("simulation returned detached management snapshot")
	}
}
