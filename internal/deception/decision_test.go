package deception

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alterhive/alterhive/internal/llm"
)

// fakeDecideLLM implements CompletionClient for decision tests. Extra
// responses (when set) are consumed in order before falling back to content.
type fakeDecideLLM struct {
	active    bool
	content   string
	err       error
	responses []string

	requests int
	lastReq  llm.CompletionRequest
}

func (f *fakeDecideLLM) IsActive() bool { return f.active }

func (f *fakeDecideLLM) Complete(_ context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	f.requests++
	f.lastReq = req
	if f.err != nil {
		return nil, f.err
	}
	if len(f.responses) > 0 {
		next := f.responses[0]
		f.responses = f.responses[1:]
		return &llm.CompletionResponse{Content: next}, nil
	}
	return &llm.CompletionResponse{Content: f.content}, nil
}

func TestDecisionRequestJSONRoundTrip(t *testing.T) {
	req := DecisionRequest{
		SessionID:   "sess-1",
		CallSite:    SiteComplexRender,
		Command:     "nmap -sV 10.0.0.0/24",
		UserContent: "recent commands",
		Terminal: &TerminalRenderInput{
			Hostname:        "staging-web-01",
			User:            "agent",
			CWD:             "/",
			Seed:            42,
			ScanVariant:     2,
			StructuredFacts: "facts",
		},
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var out DecisionRequest
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.CallSite != SiteComplexRender || out.Terminal == nil || out.Terminal.Seed != 42 {
		t.Fatalf("round trip lost fields: %#v", out)
	}

	resp := DecisionResponse{
		OK:      true,
		Content: "output",
		Intent:  &IntentResult{IntentType: "finance_hunt", Source: "llm_intent_router"},
	}
	b, _ = json.Marshal(resp)
	var out2 DecisionResponse
	if err := json.Unmarshal(b, &out2); err != nil {
		t.Fatal(err)
	}
	if out2.Intent == nil || out2.Intent.IntentType != "finance_hunt" {
		t.Fatalf("intent round trip failed: %#v", out2)
	}
}

func TestDecideComplexRenderSanitizesOutput(t *testing.T) {
	client := &fakeDecideLLM{active: true, content: "We need to simulate output.\nOpenSSH_8.9p1\n"}
	resp := Decide(context.Background(), client, DecisionRequest{
		CallSite:    SiteComplexRender,
		UserContent: "user body",
		Terminal: &TerminalRenderInput{
			Hostname: "h", User: "u", CWD: "/",
			Seed: 1, ScanVariant: 1,
			StructuredFacts: "facts", TopologyContext: "topo",
		},
	})
	if !resp.OK {
		t.Fatalf("expected OK, got error %q", resp.Error)
	}
	if strings.Contains(resp.Content, "We need to") {
		t.Fatalf("expected reasoning stripped, got %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "OpenSSH_8.9p1") {
		t.Fatalf("expected terminal content to survive, got %q", resp.Content)
	}
	// The system prompt must carry the state pieces and the policy template.
	sys := client.lastReq.Messages[0].Content
	if !strings.Contains(sys, "RANDOMIZATION SEED: 1") || !strings.Contains(sys, "facts") {
		t.Fatalf("expected system prompt to embed terminal input, got %q", sys)
	}
	if client.lastReq.MaxTokens != 800 || client.lastReq.Temperature != 0.9 {
		t.Fatalf("expected complex render params 800/0.9, got %d/%v", client.lastReq.MaxTokens, client.lastReq.Temperature)
	}
}

func TestDecideIntentRouteParsesAndSanitizes(t *testing.T) {
	client := &fakeDecideLLM{active: true, content: `{
		"intent_type":"domain_hunt",
		"target_subnet":"10.77.8.0/24",
		"confidence":0.9,
		"should_plan":true,
		"decision":{"triggered":true,"cidr":"10.77.8.0/24","theme":"domain"}
	}`}
	resp := Decide(context.Background(), client, DecisionRequest{
		CallSite:    SiteIntentRoute,
		UserContent: "payload",
	})
	if !resp.OK || resp.Intent == nil {
		t.Fatalf("expected parsed intent, got %#v", resp)
	}
	if resp.Intent.IntentType != "domain_hunt" || resp.Intent.Source != "llm_intent_router" {
		t.Fatalf("unexpected intent %#v", resp.Intent)
	}
	if resp.Intent.TargetSubnet != "10.77.8.0/24" {
		t.Fatalf("expected sanitized subnet, got %#v", resp.Intent)
	}
}

func TestDecideWorldPatchParsesAndDefaults(t *testing.T) {
	client := &fakeDecideLLM{active: true, content: `{
		"source":"llm_world_patch_agent",
		"files":[{"path":"/opt/webapp/.env","content":"DB_PASSWORD=x"}]
	}`}
	resp := Decide(context.Background(), client, DecisionRequest{
		CallSite:    SiteWorldPatch,
		UserContent: "payload",
	})
	if !resp.OK || resp.Patch == nil {
		t.Fatalf("expected parsed patch, got %#v", resp)
	}
	if resp.Patch.Source != "llm_world_patch_agent" {
		t.Fatalf("expected source preserved, got %q", resp.Patch.Source)
	}
	if resp.Patch.Reason != "llm_async_enrichment" {
		t.Fatalf("expected default reason, got %q", resp.Patch.Reason)
	}
}

func TestDecideTopologyPlanRepairRetry(t *testing.T) {
	// First completion is malformed JSON; the internal repair call must fix it.
	client := &fakeDecideLLM{active: true, responses: []string{
		"not json at all",
		`{"plan_id":"p1","phase":"recon","shadow_path":["10.60.0.0/24"]}`,
	}}
	resp := Decide(context.Background(), client, DecisionRequest{
		CallSite:    SiteTopologyPlan,
		UserContent: "payload",
	})
	if !resp.OK || resp.Plan == nil {
		t.Fatalf("expected repaired plan, got %#v (err %q)", resp, resp.Error)
	}
	if resp.Plan.PlanID != "p1" {
		t.Fatalf("expected plan id, got %#v", resp.Plan)
	}
	if client.requests != 2 {
		t.Fatalf("expected repair retry (2 completions), got %d", client.requests)
	}
}

func TestDecideInactiveClient(t *testing.T) {
	client := &fakeDecideLLM{active: false}
	resp := Decide(context.Background(), client, DecisionRequest{CallSite: SiteIntentRoute})
	if resp.OK || resp.Error != "llm inactive" {
		t.Fatalf("expected inactive error, got %#v", resp)
	}
	if client.requests != 0 {
		t.Fatalf("inactive client must not be called, got %d requests", client.requests)
	}
}

func TestLocalDecisionClientSurfacesErrors(t *testing.T) {
	client := &fakeDecideLLM{active: true, err: errors.New("provider down")}
	local := AsDecisionClient(client)
	if !local.IsActive() {
		t.Fatal("expected active local client")
	}
	_, err := local.Decide(context.Background(), DecisionRequest{CallSite: SiteIntentRoute, UserContent: "x"})
	if err == nil || !strings.Contains(err.Error(), "provider down") {
		t.Fatalf("expected surfaced error, got %v", err)
	}
}

func TestAsDecisionClientNil(t *testing.T) {
	if AsDecisionClient(nil) != nil {
		t.Fatal("nil must map to nil so disabled call sites keep working")
	}
}

func unixDecisionTestServer(t *testing.T, h http.Handler) (*RemoteDecisionClient, *httptest.Server) {
	t.Helper()
	t.Setenv("ALTERHIVE_ALLOW_LLM_CONTEXT_EXPORT", "true")
	// macOS Unix paths must remain shorter than sockaddr_un.sun_path.
	dir, err := os.MkdirTemp("", "ah-uds-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "decision.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	client, err := NewRemoteDecisionClient(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client, srv
}
func TestRemoteDecisionClientUnixAndBackoff(t *testing.T) {
	remote, srv := unixDecisionTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("sensor sent credential")
		}
		json.NewEncoder(w).Encode(DecisionResponse{OK: true, Content: "remote output"})
	}))
	resp, err := remote.Decide(context.Background(), DecisionRequest{CallSite: SiteLLMFallback})
	if err != nil || resp.Content != "remote output" {
		t.Fatalf("%+v %v", resp, err)
	}
	srv.Close()
	remote.Close()
	if _, err := remote.Decide(context.Background(), DecisionRequest{CallSite: SiteLLMFallback}); err == nil {
		t.Fatal("expected connectivity failure")
	}
	if remote.IsActive() {
		t.Fatal("expected backoff")
	}
}

func TestRemoteDecisionClientHonorsContextDeadline(t *testing.T) {
	remote, srv := unixDecisionTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(DecisionResponse{OK: true})
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := remote.Decide(ctx, DecisionRequest{CallSite: SiteLLMFallback}); err == nil {
		t.Fatal("expected deadline error")
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("request outlived its context deadline: %v", elapsed)
	}
}

func TestDecisionTimeoutFor(t *testing.T) {
	if DecisionTimeoutFor(SiteComplexRender) != 8*time.Second {
		t.Fatal("render sites must use the 8s budget")
	}
	if DecisionTimeoutFor(SiteTopologyPlan) != 60*time.Second {
		t.Fatal("planner sites must use the 60s budget")
	}
}

// TestLocalClientMatchesDirectHandler documents the equivalence property the
// split relies on: the sensor's in-process path and the manager's HTTP path
// both execute Decide, so standalone and split deployments share one decision
// implementation by construction.
func TestLocalClientMatchesDirectHandler(t *testing.T) {
	client := &fakeDecideLLM{active: true, content: "plain output\n"}
	req := DecisionRequest{
		CallSite:    SiteLLMFallback,
		UserContent: "body",
		Terminal:    &TerminalRenderInput{Hostname: "h", User: "u", CWD: "/"},
	}
	direct := Decide(context.Background(), client, req)
	viaLocal, err := AsDecisionClient(client).Decide(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if direct.OK != viaLocal.OK || direct.Content != viaLocal.Content {
		t.Fatalf("local client diverged from direct handler: %#v vs %#v", direct, viaLocal)
	}
}

func TestSanitizeTerminalOutputMoved(t *testing.T) {
	got := SanitizeTerminalOutput("```bash\nreal line\n```\nAs an AI model disclaimer")
	if strings.Contains(got, "```") || strings.Contains(got, "As an AI") {
		t.Fatalf("expected fences and disclaimers stripped, got %q", got)
	}
}

func TestContextExportGateBlocksEverySiteBeforeTransport(t *testing.T) {
	t.Setenv("ALTERHIVE_ALLOW_LLM_CONTEXT_EXPORT", "false")
	localBackend := &fakeDecideLLM{active: true}
	local := AsDecisionClient(localBackend)
	remote, err := NewRemoteDecisionClient("/nonexistent/manager.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	for _, site := range []CallSite{SiteComplexRender, SiteLLMFallback, SiteWorldPatch, SiteIntentRoute, SiteTopologyPlan, SiteDeceptionPlan, SitePlanRepair} {
		for _, client := range []DecisionClient{local, remote} {
			if client.IsActive() {
				t.Fatal("disabled export still active")
			}
			if _, err := client.Decide(context.Background(), DecisionRequest{CallSite: site}); err == nil || !strings.Contains(err.Error(), "export disabled") {
				t.Fatalf("site %s: %v", site, err)
			}
		}
	}
	if localBackend.requests != 0 || !remote.lastFailed.IsZero() {
		t.Fatal("disabled export reached backend/transport")
	}
}

func TestManagerThrottlingIsNotConnectivityFailure(t *testing.T) {
	remote, srv := unixDecisionTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	defer remote.Close()
	if _, err := remote.Decide(context.Background(), DecisionRequest{CallSite: SiteLLMFallback}); err == nil {
		t.Fatal("expected throttled error")
	}
	if !remote.lastFailed.IsZero() || remote.IsActive() {
		t.Fatal("wrong throttling state")
	}
	remote.mu.Lock()
	remote.retryAfter = time.Now().Add(-time.Second)
	remote.mu.Unlock()
	if !remote.IsActive() {
		t.Fatal("rate limited client stuck in 30 second connectivity backoff")
	}
}
