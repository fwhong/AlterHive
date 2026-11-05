package deception

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alterhive/alterhive/internal/domain"
	"github.com/alterhive/alterhive/internal/llm"
)

type controlledDecision struct {
	requests chan DecisionRequest
	release  chan DecisionResponse
}

func (d *controlledDecision) IsActive() bool { return true }
func (d *controlledDecision) Decide(ctx context.Context, req DecisionRequest) (DecisionResponse, error) {
	select {
	case d.requests <- req:
	case <-ctx.Done():
		return DecisionResponse{}, ctx.Err()
	}
	select {
	case result := <-d.release:
		return result, nil
	case <-ctx.Done():
		return DecisionResponse{}, ctx.Err()
	}
}
func newControlledDecision() *controlledDecision {
	return &controlledDecision{make(chan DecisionRequest, 10), make(chan DecisionResponse, 10)}
}
func waitMutation(t *testing.T, s *domain.SessionContext, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.ApplyPending()
		if ready() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("cold-path mutation did not reach hot path")
}
func TestEventBusPlannerUsesLiveSessionAndAppliesPlan(t *testing.T) {
	d := newControlledDecision()
	top := domain.NewVirtualTopology(domain.TopologyConfig{CIDR: "192.168.56.0/24", Hosts: []domain.VirtualHost{{IP: "192.168.56.10", Hostname: "jump01"}}})
	safety := domain.NewSafetyPolicy("192.168.56.0/24")
	o := NewAgentOrchestrator(top, d, safety)
	s := domain.NewSessionContext("agent", "127.0.0.1:1")
	o.eventBus.Publish(PlanningEvent{Session: s, SessionID: s.SessionID, Type: EventTargetShift, Command: "nmap 10.44.5.0/24", Payload: map[string]interface{}{"subnet": "10.44.5.0/24"}})
	var req DecisionRequest
	select {
	case req = <-d.requests:
	case <-time.After(time.Second):
		t.Fatal("event bus never called planner")
	}
	if req.CallSite != SiteDeceptionPlan || req.SessionID != s.SessionID || req.UserContent == "" {
		t.Fatalf("missing session snapshot: %#v", req)
	}
	plan := PlanFromDecision(ExpansionDecision{Triggered: true, CIDR: "10.44.5.0/24", PivotIP: "192.168.56.10", Theme: "network"})
	if top.GetHostForSession("10.44.5.10", s.SessionID) != nil {
		t.Fatal("unexpected pre-completion mutation")
	}
	d.release <- DecisionResponse{OK: true, Plan: &DeceptionPlan{PlanID: "event-regression", LegacyPlan: plan}}
	waitMutation(t, s, func() bool { return top.GetHostForSession("10.44.5.10", s.SessionID) != nil })
	if !safety.IsVirtualIP("10.44.5.10") {
		t.Fatal("async topology missing virtual command policy")
	}
}
func TestAsyncWorldPatchConsumedAndVisibilityGateEnforced(t *testing.T) {
	d := newControlledDecision()
	s := domain.NewSessionContext("agent", "127.0.0.1:1")
	world := domain.NewWorldState()
	a := LLMWorldPatchAgent{decider: d}
	a.Plan(s, world, "grep token /opt/webapp/logs/*.log", nil)
	select {
	case <-d.requests:
	case <-time.After(time.Second):
		t.Fatal("no world patch request")
	}
	d.release <- DecisionResponse{OK: true, Patch: &WorldPatch{Source: "regression", Files: []FileMutation{{Path: "/tmp/regression-clue.txt", Content: "breadcrumb\n", VisibleAfter: []string{GateReconObserved}}}}}
	waitMutation(t, s, func() bool { return s.Planning.HasCandidateFile("/tmp/regression-clue.txt") })
	if world.GetFileEntry("/tmp/regression-clue.txt") != nil {
		t.Fatal("visibility gate bypassed")
	}
	s.UnlockAccessState(GateReconObserved)
	promoteCandidateFiles(s, world, "cat /tmp/regression-clue.txt")
	if world.GetFileEntry("/tmp/regression-clue.txt") == nil {
		t.Fatal("async candidate never promoted")
	}
}
func TestAsyncWorldPatchStillRunsSafetyCritic(t *testing.T) {
	d := newControlledDecision()
	s := domain.NewSessionContext("agent", "127.0.0.1:1")
	world := domain.NewWorldState()
	a := LLMWorldPatchAgent{decider: d}
	done := make(chan struct{})
	go func() { a.planAsync(s, world, "cat /tmp/clue", DecisionRequest{}); close(done) }()
	<-d.requests
	d.release <- DecisionResponse{OK: true, Patch: &WorldPatch{Files: []FileMutation{{Path: "/tmp/flag.txt", Content: "flag{win}"}}}}
	<-done
	s.ApplyPending()
	if world.GetFileEntry("/tmp/flag.txt") != nil || s.Planning.HasCandidateFile("/tmp/flag.txt") {
		t.Fatal("async patch bypassed critic")
	}
}
func TestLocalClientHandlesTypedNil(t *testing.T) {
	var manager *llm.Manager
	if AsDecisionClient(manager).IsActive() {
		t.Fatal("typed nil manager active")
	}
}

func TestRejectedAsyncPlanRepairsOnceAndRevalidates(t *testing.T) {
	d := newControlledDecision()
	s := domain.NewSessionContext("agent", "127.0.0.1:1")
	top := domain.NewVirtualTopology(domain.TopologyConfig{CIDR: "192.168.56.0/24", Hosts: []domain.VirtualHost{{IP: "192.168.56.10", Hostname: "jump01"}}})
	p := NewTopologyPlanner(top, d)
	original := DecisionRequest{SessionID: s.SessionID, Command: "nmap 10.44.5.0/24", UserContent: "frozen context"}
	p.acceptAsyncPlan(s, original, DeceptionPlan{PlanID: "empty-invalid"}, true)
	select {
	case req := <-d.requests:
		if req.CallSite != SitePlanRepair || !strings.Contains(req.UserContent, "frozen context") {
			t.Fatal(req)
		}
	case <-time.After(time.Second):
		t.Fatal("missing repair request")
	}
	valid := PlanFromDecision(ExpansionDecision{Triggered: true, CIDR: "10.44.5.0/24", PivotIP: "192.168.56.10", Theme: "network"})
	d.release <- DecisionResponse{OK: true, Plan: &DeceptionPlan{PlanID: "repaired", LegacyPlan: valid}}
	waitMutation(t, s, func() bool { return top.GetHostForSession("10.44.5.10", s.SessionID) != nil })
	p.acceptAsyncPlan(s, original, DeceptionPlan{PlanID: "still-invalid"}, false)
	select {
	case <-d.requests:
		t.Fatal("unbounded repair loop")
	case <-time.After(10 * time.Millisecond):
	}
}
