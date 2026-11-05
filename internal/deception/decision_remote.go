package deception

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/alterhive/alterhive/internal/ipc"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// RemoteDecisionClient is the sensor-side DecisionClient. It forwards cold
// path decisions to the central manager over HTTP. The sensor process holds
// no LLM credentials and makes no provider network calls: its only decision
// egress is a UID-authenticated Unix socket.
type RemoteDecisionClient struct {
	socket string
	client *http.Client

	mu         sync.Mutex
	lastFailed time.Time
	retryAfter time.Time
}

const remoteDecisionBackoff = 30 * time.Second

// NewRemoteDecisionClient only accepts an absolute Unix socket path. There is
// no TCP fallback, bearer credential, environment proxy or DNS lookup.
func NewRemoteDecisionClient(socket string) (*RemoteDecisionClient, error) {
	if !filepath.IsAbs(socket) {
		return nil, fmt.Errorf("absolute decision socket path required")
	}
	return &RemoteDecisionClient{socket: socket, client: ipc.Client(socket)}, nil
}

// IsActive reports whether a manager is configured and not in failure
// backoff. Cold path call sites use this the same way they previously used
// the local LLM IsActive check.
func (r *RemoteDecisionClient) IsActive() bool {
	if r == nil || r.socket == "" || !allowLLMContextExport() {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Since(r.lastFailed) >= remoteDecisionBackoff && !time.Now().Before(r.retryAfter)
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
	if r == nil || r.socket == "" {
		return DecisionResponse{}, fmt.Errorf("manager socket not configured")
	}

	if !allowLLMContextExport() {
		return DecisionResponse{}, fmt.Errorf("LLM context export disabled")
	}
	if !r.IsActive() {
		return DecisionResponse{}, fmt.Errorf("manager connectivity backoff")
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

	httpReq, err := http.NewRequestWithContext(httpCtx, http.MethodPost, "http://manager/v1/decide", bytes.NewReader(body))
	if err != nil {
		r.markFailed()
		return DecisionResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(httpReq)
	if err != nil {
		r.markFailed()
		return DecisionResponse{}, fmt.Errorf("manager unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		seconds, err := strconv.Atoi(resp.Header.Get("Retry-After"))
		if err != nil || seconds < 1 {
			seconds = 1
		}
		if seconds > 3600 {
			seconds = 3600
		}
		r.mu.Lock()
		r.lastFailed = time.Time{}
		r.retryAfter = time.Now().Add(time.Duration(seconds) * time.Second)
		r.mu.Unlock()
		return DecisionResponse{}, fmt.Errorf("manager throttled")
	}
	if resp.StatusCode != http.StatusOK {
		// Non-200 means the manager itself is unhealthy (or auth failed):
		// a connectivity-level problem, so the backoff applies.
		r.markFailed()
		return DecisionResponse{}, fmt.Errorf("manager returned HTTP %d", resp.StatusCode)
	}

	var out DecisionResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&out); err != nil {
		r.markFailed()
		return DecisionResponse{}, fmt.Errorf("decode manager response: %w", err)
	}
	r.markAlive()
	if !out.OK {
		// Manager reached a decision-side failure (provider error, parse
		// failure): not a connectivity problem, so no backoff.
		return out, fmt.Errorf("decision %s: %s", req.CallSite, out.Error)
	}
	r.markAlive()
	return out, nil
}

func (r *RemoteDecisionClient) Close() { r.client.CloseIdleConnections() }
