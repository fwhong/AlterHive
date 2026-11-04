package deception

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RemoteDecisionClient is the sensor-side DecisionClient. It forwards cold
// path decisions to the central manager over HTTP. The sensor process holds
// no LLM credentials and makes no provider network calls: its only decision
// egress is managerURL.
type RemoteDecisionClient struct {
	baseURL string
	token   string

	mu         sync.Mutex
	lastFailed time.Time
}

const remoteDecisionBackoff = 30 * time.Second

// NewRemoteDecisionClient creates the sensor→manager decision client.
// managerURL is the manager base URL (e.g. "http://alterhive-manager:8100").
func NewRemoteDecisionClient(managerURL, token string) *RemoteDecisionClient {
	return &RemoteDecisionClient{
		baseURL: strings.TrimRight(managerURL, "/"),
		token:   token,
	}
}

// IsActive reports whether a manager is configured and not in failure
// backoff. Cold path call sites use this the same way they previously used
// the local LLM IsActive check.
func (r *RemoteDecisionClient) IsActive() bool {
	if r == nil || r.baseURL == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Since(r.lastFailed) >= remoteDecisionBackoff
}

func (r *RemoteDecisionClient) markFailed() {
	r.mu.Lock()
	r.lastFailed = time.Now()
	r.mu.Unlock()
}

func (r *RemoteDecisionClient) markAlive() {
	r.mu.Lock()
	r.lastFailed = time.Time{}
	r.mu.Unlock()
}

// Decide sends a decision request to the manager. The context deadline set
// by the sensor call site (8s render / 60s plan) becomes the HTTP timeout.
// Transport errors, HTTP errors and manager-side failures all surface as a
// Go error so existing call sites degrade exactly like a local LLM failure.
func (r *RemoteDecisionClient) Decide(ctx context.Context, req DecisionRequest) (DecisionResponse, error) {
	if r == nil || r.baseURL == "" {
		return DecisionResponse{}, fmt.Errorf("manager URL not configured")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return DecisionResponse{}, err
	}

	timeout := DecisionTimeoutFor(req.CallSite)
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 && remaining < timeout {
			timeout = remaining
		}
	}
	httpCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(httpCtx, http.MethodPost, r.baseURL+"/v1/decide", bytes.NewReader(body))
	if err != nil {
		r.markFailed()
		return DecisionResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if r.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+r.token)
	}

	resp, err := sharedHTTPClient().Do(httpReq)
	if err != nil {
		r.markFailed()
		return DecisionResponse{}, fmt.Errorf("manager unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Non-200 means the manager itself is unhealthy (or auth failed):
		// a connectivity-level problem, so the backoff applies.
		r.markFailed()
		return DecisionResponse{}, fmt.Errorf("manager returned HTTP %d", resp.StatusCode)
	}

	var out DecisionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		r.markFailed()
		return DecisionResponse{}, fmt.Errorf("decode manager response: %w", err)
	}
	if !out.OK {
		// Manager reached a decision-side failure (provider error, parse
		// failure): not a connectivity problem, so no backoff.
		return out, fmt.Errorf("decision %s: %s", req.CallSite, out.Error)
	}
	r.markAlive()
	return out, nil
}

var (
	httpClientOnce sync.Once
	httpClient     *http.Client
)

// sharedHTTPClient reuses connections to the manager across decisions.
func sharedHTTPClient() *http.Client {
	httpClientOnce.Do(func() {
		httpClient = &http.Client{Transport: &http.Transport{
			MaxIdleConns:        16,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
		}}
	})
	return httpClient
}
