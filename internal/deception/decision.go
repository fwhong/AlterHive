package deception

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/alterhive/alterhive/internal/llm"
)

// CallSite identifies which LLM decision point a request belongs to. The
// manager dispatches on this value to pick the prompt policy, model params
// and response parser. Everything except the state-derived UserContent lives
// manager-side, so deception policy can evolve without redeploying sensors.
type CallSite string

const (
	// SiteComplexRender rewrites deterministic rule-engine output for complex
	// commands (rule_engine.handleComplexAsync).
	SiteComplexRender CallSite = "complex_render"
	// SiteLLMFallback authors terminal output for commands no rule handled
	// (rule_engine.llmFallbackAsync).
	SiteLLMFallback CallSite = "llm_fallback"
	// SiteWorldPatch generates async WorldPatch enrichment
	// (LLMWorldPatchAgent.planAsync).
	SiteWorldPatch CallSite = "world_patch"
	// SiteIntentRoute is the LLM-enhanced intent classification
	// (IntentRouterAgent.routeWithLLM).
	SiteIntentRoute CallSite = "intent_route"
	// SiteTopologyPlan generates a DeceptionPlan (TopologyPlanner.llmPlan),
	// with the internal JSON-repair retry enabled.
	SiteTopologyPlan CallSite = "topology_plan"
	// SiteDeceptionPlan generates a DeceptionPlan (DeceptionPlannerAgent.Plan).
	SiteDeceptionPlan CallSite = "deception_plan"
	// SitePlanRepair revises a rejected DeceptionPlan
	// (TopologyPlanner.repairRejectedPlan / DeceptionPlannerAgent.RepairPlan).
	SitePlanRepair CallSite = "plan_repair"
)

// TerminalRenderInput carries the state-derived pieces of the two terminal
// rendering sites. The system-prompt template itself lives in Decide (manager
// side); the sensor only serializes its live session/world state into these
// fields.
type TerminalRenderInput struct {
	Hostname string `json:"hostname"`
	User     string `json:"user"`
	CWD      string `json:"cwd"`

	// Complex-render only.
	Seed         int `json:"seed"`
	ScanVariant  int `json:"scan_variant"`
	StructuredFacts string `json:"structured_facts,omitempty"`

	TopologyContext  string `json:"topology_context"`
	ProtectedTargets string `json:"protected_targets"`
	MemorySummary    string `json:"memory_summary"`
	StrategyPrompt   string `json:"strategy_prompt"`
}

// DecisionRequest is the sensor→manager decision call. UserContent is the
// state-derived prompt body the sensor serializes from its own session,
// world and topology state.
type DecisionRequest struct {
	SessionID   string              `json:"session_id"`
	CallSite    CallSite            `json:"call_site"`
	Command     string              `json:"command"`
	UserContent string              `json:"user_content"`
	Terminal    *TerminalRenderInput `json:"terminal,omitempty"`
}

// DecisionResponse is the manager→sensor decision result. Artifacts are
// already parsed and sanitized manager-side; the sensor keeps only its
// safety guards (GuardTerminalOutput / GuardResponseFacts) and application
// logic.
type DecisionResponse struct {
	OK      bool           `json:"ok"`
	Error   string         `json:"error,omitempty"`
	Content string         `json:"content,omitempty"` // raw/sanitized terminal output
	Intent  *IntentResult  `json:"intent,omitempty"`
	Plan    *DeceptionPlan `json:"plan,omitempty"`
	Patch   *WorldPatch    `json:"patch,omitempty"`
}

// DecisionClient is the decision surface consumed by the hot-path components.
// In standalone mode it is a LocalDecisionClient wrapping the in-process
// llm.Manager; in sensor mode it is a RemoteDecisionClient talking to the
// central manager over HTTP.
type DecisionClient interface {
	IsActive() bool
	Decide(ctx context.Context, req DecisionRequest) (DecisionResponse, error)
}

// decisionClientInactive reports whether a decision client is unusable
// (nil interface or inactive). Mirrors completionClientInactive semantics.
func decisionClientInactive(client DecisionClient) bool {
	if client == nil {
		return true
	}
	if _, ok := client.(*LocalDecisionClient); ok {
		return false
	}
	return !client.IsActive()
}

// AsDecisionClient wraps a plain CompletionClient (llm.Manager, test fakes)
// into a DecisionClient running Decide in-process. A nil input maps to nil so
// existing "LLM disabled" call sites keep working.
func AsDecisionClient(client CompletionClient) DecisionClient {
	if client == nil {
		return nil
	}
	return &LocalDecisionClient{llm: client}
}

// LocalDecisionClient executes Decide against an in-process completion
// client. Standalone mode and tests use this so split and standalone behave
// identically by construction.
type LocalDecisionClient struct {
	llm CompletionClient
}

// NewLocalDecisionClient creates a DecisionClient around a CompletionClient.
func NewLocalDecisionClient(client CompletionClient) DecisionClient {
	return AsDecisionClient(client)
}

func (l *LocalDecisionClient) IsActive() bool {
	if l == nil || l.llm == nil {
		return false
	}
	return l.llm.IsActive()
}

func (l *LocalDecisionClient) Decide(ctx context.Context, req DecisionRequest) (DecisionResponse, error) {
	resp := Decide(ctx, l.llm, req)
	if !resp.OK {
		return resp, fmt.Errorf("decision %s: %s", req.CallSite, resp.Error)
	}
	return resp, nil
}

// Decide executes a site's LLM decision: prompt policy, model params,
// completion and response parsing/repair. It is the single decision entry
// point shared by standalone (in-process) and manager (HTTP) deployments.
func Decide(ctx context.Context, client CompletionClient, req DecisionRequest) DecisionResponse {
	if completionClientInactive(client) {
		return DecisionResponse{OK: false, Error: "llm inactive"}
	}

	switch req.CallSite {
	case SiteComplexRender:
		return decideComplexRender(ctx, client, req)
	case SiteLLMFallback:
		return decideLLMFallback(ctx, client, req)
	case SiteIntentRoute:
		return decideIntentRoute(ctx, client, req)
	case SiteWorldPatch:
		return decideWorldPatch(ctx, client, req)
	case SiteTopologyPlan:
		return decidePlan(ctx, client, req, true)
	case SiteDeceptionPlan, SitePlanRepair:
		return decidePlan(ctx, client, req, false)
	default:
		return DecisionResponse{OK: false, Error: fmt.Sprintf("unknown call site %q", req.CallSite)}
	}
}

// --- Terminal rendering sites (engine) ---

func decideComplexRender(ctx context.Context, client CompletionClient, req DecisionRequest) DecisionResponse {
	t := req.Terminal
	if t == nil {
		return DecisionResponse{OK: false, Error: "terminal render input required"}
	}

	systemContent := fmt.Sprintf(`You are a Linux terminal on host %s (Ubuntu 22.04). User: %s. CWD: %s.
An attacker (AI pentest agent or human) just ran a network/service command. You must produce the exact raw terminal output that command would print.

RANDOMIZATION SEED: %d  VARIANT: %d

COMMAND FACTS (authoritative ground truth — USE these facts, do NOT COPY them verbatim):
%s

CRITICAL: Author fresh terminal output. Do NOT echo the fact strings. Vary with the seed:
- nmap version: pick from [7.92, 7.93, 7.94, 7.95] based on variant %% 4
- latency: 0.0012s-0.0089s range, jittered by seed
- scan time: 1.8s-4.5s range, jittered by seed
- timestamp format: vary between ISO 8601 and Unix-style
- spacing/alignment: vary column widths slightly
- banner strings: use realistic variations (e.g. "OpenSSH 8.9p1" vs "OpenSSH 8.4p1")

Return ONLY raw terminal output. No markdown, fences, prompts, or analysis.
Every IP/port/service from the facts MUST appear. Do not invent new facts.
Keep output under 40 lines.
Topology: %s
Protected targets already known to the attacker: %s
%s
%s`, t.Hostname, t.User, t.CWD, t.Seed, t.ScanVariant, t.StructuredFacts, t.TopologyContext, t.ProtectedTargets, t.MemorySummary, t.StrategyPrompt)

	completeReq := llm.CompletionRequest{
		MaxTokens:   800,
		Temperature: 0.9,
		Messages: []llm.Message{
			{Role: "system", Content: systemContent},
			{Role: "user", Content: req.UserContent},
		},
	}

	resp, err := client.Complete(ctx, completeReq)
	if err != nil {
		return DecisionResponse{OK: false, Error: err.Error()}
	}
	return DecisionResponse{OK: true, Content: SanitizeTerminalOutput(resp.Content)}
}

func decideLLMFallback(ctx context.Context, client CompletionClient, req DecisionRequest) DecisionResponse {
	t := req.Terminal
	if t == nil {
		return DecisionResponse{OK: false, Error: "terminal render input required"}
	}

	systemContent := fmt.Sprintf(`You are a Linux terminal on host %s (Ubuntu 22.04). User: %s. CWD: %s.
Rules:
- Return ONLY raw terminal output. No markdown, no fences, no prompts, no explanations.
- Never explain how you chose the output. Never write analysis, reasoning, or phrases like "we need to".
- If a file does not exist, generate realistic content for a staging web server.
- If a command needs root and user is not root, return "Permission denied".
- For "ls" on a directory, list realistic files. For "cat", show realistic file content.
- Do not invent new IPs, hostnames, services, credentials, routes, or successful exploit facts.
- You may only mention IPs present in the approved topology or protected target list.
- Keep output under 15 lines. Be terse like a real terminal.
Topology: %s
Protected targets already known to the attacker: %s
%s
%s`, t.Hostname, t.User, t.CWD, t.TopologyContext, t.ProtectedTargets, t.MemorySummary, t.StrategyPrompt)

	completeReq := llm.CompletionRequest{
		MaxTokens:   500,
		Temperature: 0.3,
		Messages: []llm.Message{
			{Role: "system", Content: systemContent},
			{Role: "user", Content: req.UserContent},
		},
	}

	resp, err := client.Complete(ctx, completeReq)
	if err != nil {
		return DecisionResponse{OK: false, Error: err.Error()}
	}
	return DecisionResponse{OK: true, Content: SanitizeTerminalOutput(resp.Content)}
}

// --- Intent routing site ---

func decideIntentRoute(ctx context.Context, client CompletionClient, req DecisionRequest) DecisionResponse {
	completeReq := llm.CompletionRequest{
		MaxTokens:      700,
		Temperature:    0.0,
		ResponseFormat: "json_object",
		Messages: []llm.Message{
			{Role: "system", Content: buildIntentRouterSystemPrompt()},
			{Role: "user", Content: req.UserContent},
		},
	}

	resp, err := client.Complete(ctx, completeReq)
	if err != nil {
		return DecisionResponse{OK: false, Error: err.Error()}
	}
	if strings.TrimSpace(resp.Content) == "" {
		return DecisionResponse{OK: false, Error: "empty LLM intent response"}
	}

	var result IntentResult
	if err := json.Unmarshal([]byte(resp.Content), &result); err != nil {
		return DecisionResponse{OK: false, Error: fmt.Sprintf("parse LLM intent: %v", err)}
	}
	result.Source = "llm_intent_router"

	sanitized, err := sanitizeLLMIntent(result)
	if err != nil {
		return DecisionResponse{OK: false, Error: err.Error()}
	}
	return DecisionResponse{OK: true, Intent: &sanitized}
}

// --- World patch site ---

func decideWorldPatch(ctx context.Context, client CompletionClient, req DecisionRequest) DecisionResponse {
	completeReq := llm.CompletionRequest{
		MaxTokens:      900,
		Temperature:    0.25,
		ResponseFormat: "json_object",
		Messages: []llm.Message{
			{Role: "system", Content: buildWorldPatchSystemPrompt()},
			{Role: "user", Content: req.UserContent},
		},
	}

	resp, err := client.Complete(ctx, completeReq)
	if err != nil {
		return DecisionResponse{OK: false, Error: err.Error()}
	}
	if strings.TrimSpace(resp.Content) == "" {
		return DecisionResponse{OK: false, Error: "empty LLM world patch response"}
	}

	patch, err := parseWorldPatchContent(resp.Content)
	if err != nil || patch.Source == "" {
		return DecisionResponse{OK: false, Error: fmt.Sprintf("parse world patch: %v", err)}
	}
	patch = sanitizeWorldPatch(patch)
	patch.Source = "llm_world_patch_agent"
	if patch.Reason == "" {
		patch.Reason = "llm_async_enrichment"
	}
	return DecisionResponse{OK: true, Patch: &patch}
}

// --- Deception plan sites ---

func decidePlan(ctx context.Context, client CompletionClient, req DecisionRequest, withJSONRepair bool) DecisionResponse {
	completeReq := llm.CompletionRequest{
		MaxTokens:      1600,
		Temperature:    0.1,
		ResponseFormat: "json_object",
		Messages: []llm.Message{
			{Role: "system", Content: buildPlannerSystemPrompt()},
			{Role: "user", Content: req.UserContent},
		},
	}

	resp, err := client.Complete(ctx, completeReq)
	if err != nil {
		return DecisionResponse{OK: false, Error: err.Error()}
	}
	if strings.TrimSpace(resp.Content) == "" {
		return DecisionResponse{OK: false, Error: "empty LLM planner response"}
	}

	plan, err := parseDeceptionPlanContent(resp.Content)
	if err != nil {
		if !withJSONRepair {
			return DecisionResponse{OK: false, Error: fmt.Sprintf("parse deception plan: %v", err)}
		}
		repaired, repairErr := repairPlanJSON(ctx, client, resp.Content)
		if repairErr != nil {
			return DecisionResponse{OK: false, Error: fmt.Sprintf("parse deception plan: %v; repair failed: %v", err, repairErr)}
		}
		plan, err = parseDeceptionPlanContent(repaired)
		if err != nil {
			return DecisionResponse{OK: false, Error: fmt.Sprintf("parse repaired deception plan: %v", err)}
		}
	}
	return DecisionResponse{OK: true, Plan: &plan}
}

// repairPlanJSON asks the LLM to coerce a malformed plan response into one
// valid JSON object. Manager-internal: only the decision side uses it.
func repairPlanJSON(ctx context.Context, client CompletionClient, raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("empty raw planner response")
	}
	req := llm.CompletionRequest{
		MaxTokens:      1200,
		Temperature:    0,
		ResponseFormat: "json_object",
		Messages: []llm.Message{
			{Role: "system", Content: "Repair this AlterHive TopologyPlan into one valid JSON object only. Do not add markdown or commentary. Preserve fields and intent; do not create protected targets."},
			{Role: "user", Content: raw},
		},
	}
	resp, err := client.Complete(ctx, req)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(resp.Content) == "" {
		return "", fmt.Errorf("empty repair response")
	}
	return resp.Content, nil
}

// --- Terminal output sanitization (shared with the engine) ---

// SanitizeTerminalOutput strips markdown fences, echoed prompt artifacts and
// AI disclaimers from an LLM-authored terminal output. Moved verbatim from
// the engine so the decision side can apply it before returning Content.
func SanitizeTerminalOutput(output string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return ""
	}

	// Strip markdown code fences (```python, ```bash, ``` etc.)
	lines := strings.Split(output, "\n")
	var cleaned []string
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Detect opening/closing fences
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		// Skip prompt artifacts the LLM echoed back
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "current files near cwd:") {
			break // Everything after this is prompt context, not output
		}
		if strings.HasPrefix(lower, "command:") && len(trimmed) < 80 {
			break
		}
		if strings.HasPrefix(lower, "command history tail:") {
			break
		}
		if strings.HasPrefix(lower, "evidence:") && len(trimmed) < 200 {
			break
		}
		if strings.HasPrefix(lower, "access states:") {
			break
		}
		if strings.HasPrefix(lower, "ppf triggered:") {
			break
		}
		if strings.HasPrefix(lower, "memory:") && len(trimmed) < 200 {
			break
		}
		// Skip standalone language identifiers
		if trimmed == "python" || trimmed == "bash" || trimmed == "sh" || trimmed == "shell" {
			continue
		}
		// Skip AI disclaimers
		if strings.Contains(lower, "as an ai") || strings.Contains(lower, "as a language model") {
			return ""
		}
		if looksLikeLLMReasoning(trimmed) {
			continue
		}
		cleaned = append(cleaned, line)
	}
	_ = inFence

	output = strings.Join(cleaned, "\n")
	output = strings.TrimSpace(output)
	if output == "" {
		return ""
	}
	return strings.TrimRight(output, "\n") + "\n"
}

func looksLikeLLMReasoning(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	if lower == "" {
		return false
	}
	reasoningPrefixes := []string{
		"we need to",
		"i need to",
		"i'll ",
		"the user is",
		"according to",
		"consider ",
		"example:",
		"example content",
		"better:",
		"alternatively",
		"but ",
		"maybe ",
		"that includes",
		"given the strategy",
		"the rules ",
		"this command ",
		"thus,",
		"output:",
		"make it ",
		"we should",
		"i should",
		"let's",
		"so ",
	}
	for _, prefix := range reasoningPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return strings.Contains(lower, "return only") ||
		strings.Contains(lower, "simulate the output") ||
		strings.Contains(lower, "plausible output") ||
		strings.Contains(lower, "terminal output")
}

// DecisionTimeoutFor returns the per-site budget the decision side enforces.
// Callers on the sensor already set their own context deadlines (8s render /
// 60s plan); the manager applies the same values when the request carries no
// deadline.
func DecisionTimeoutFor(site CallSite) time.Duration {
	switch site {
	case SiteComplexRender, SiteLLMFallback, SiteIntentRoute, SiteWorldPatch:
		return 8 * time.Second
	default:
		return 60 * time.Second
	}
}
