// Package decisionsvc is the central manager's HTTP decision service. The
// manager is the only process holding LLM provider credentials and making
// provider network calls; sensors forward cold-path decisions here and
// receive parsed, sanitized artifacts back.
package decisionsvc

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/alterhive/alterhive/internal/deception"
	"github.com/alterhive/alterhive/internal/llm"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// Server exposes the decision API and healthz for the manager process.
type Server struct {
	llm        *llm.Manager
	slots      chan struct{}
	window     time.Time
	burst      int
	mu         sync.Mutex
	day        time.Time
	spent      int
	dailyLimit int
}

// NewServer creates the decision service around an LLM manager.
func NewServer(llmMgr *llm.Manager) *Server {
	return &Server{llm: llmMgr, slots: make(chan struct{}, 4), window: time.Now(), day: time.Now(), dailyLimit: 10000}
}

// RegisterRoutes mounts the decision service routes.
func (s *Server) RegisterRoutes(r *gin.Engine) {
	r.GET("/healthz", s.healthz)
	v1 := r.Group("/v1")
	v1.POST("/decide", s.limits(), s.decide)
}

// The server must ONLY be mounted on the UID-authenticated Unix listener.
// Limit the sole local sensor as a principal, not spoofable session_id values.
func (s *Server) limits() gin.HandlerFunc {
	return func(c *gin.Context) {
		s.mu.Lock()
		if time.Since(s.window) >= time.Second {
			s.window = time.Now()
			s.burst = 0
		}
		s.burst++
		rateOK := s.burst <= 8
		s.mu.Unlock()
		if !rateOK {
			c.Header("Retry-After", "1")
			c.AbortWithStatusJSON(429, gin.H{"ok": false, "error": "decision rate limit"})
			return
		}
		select {
		case s.slots <- struct{}{}:
			defer func() { <-s.slots }()
		default:
			c.Header("Retry-After", "1")
			c.AbortWithStatusJSON(429, gin.H{"ok": false, "error": "decision concurrency limit"})
			return
		}
		s.mu.Lock()
		if time.Since(s.day) >= 24*time.Hour {
			s.day = time.Now()
			s.spent = 0
		}
		allowed := s.spent < s.dailyLimit
		if allowed {
			s.spent++
		}
		s.mu.Unlock()
		if !allowed {
			c.Header("Retry-After", "3600")
			c.AbortWithStatusJSON(429, gin.H{"ok": false, "error": "daily decision budget exceeded"})
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 256<<10)
		c.Next()
	}
}

func (s *Server) healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":     "ok",
		"llm_active": s.llm != nil && s.llm.IsActive(),
	})
}

func (s *Server) decide(c *gin.Context) {
	var req deception.DecisionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "invalid request"})
		return
	}

	// Enforce the site budget even when the sensor sent no deadline.
	ctx, cancel := context.WithTimeout(c.Request.Context(), deception.DecisionTimeoutFor(req.CallSite))
	defer cancel()

	started := time.Now()
	resp := deception.Decide(ctx, s.llm, req)
	// Upstream error payloads are never part of the sensor protocol.
	if !resp.OK {
		resp.Error = "decision unavailable"
	}

	log.WithFields(log.Fields{
		"session":   req.SessionID,
		"call_site": req.CallSite,
		"ok":        resp.OK,
		"latency":   time.Since(started).Round(time.Millisecond).String(),
	}).Info("decision handled")

	// A decision-side failure (provider error, parse failure) is a valid
	// answer to a well-formed request: always 200 so the sensor can decode
	// the body and degrade without treating it as a connectivity problem.
	c.JSON(http.StatusOK, resp)
}
