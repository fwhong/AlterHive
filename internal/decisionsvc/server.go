// Package decisionsvc is the central manager's HTTP decision service. The
// manager is the only process holding LLM provider credentials and making
// provider network calls; sensors forward cold-path decisions here and
// receive parsed, sanitized artifacts back.
package decisionsvc

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/alterhive/alterhive/internal/deception"
	"github.com/alterhive/alterhive/internal/llm"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// Server exposes the decision API and healthz for the manager process.
type Server struct {
	llm   *llm.Manager
	token string // required Bearer token; empty disables /v1/decide (fail closed)
}

// NewServer creates the decision service around an LLM manager.
func NewServer(llmMgr *llm.Manager, token string) *Server {
	return &Server{llm: llmMgr, token: token}
}

// RegisterRoutes mounts the decision service routes.
func (s *Server) RegisterRoutes(r *gin.Engine) {
	r.GET("/healthz", s.healthz)
	v1 := r.Group("/v1")
	v1.POST("/decide", s.authMiddleware(), s.decide)
}

func (s *Server) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.token == "" {
			c.JSON(http.StatusServiceUnavailable, gin.H{"ok": false, "error": "manager token not configured"})
			c.Abort()
			return
		}
		auth := c.GetHeader("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") != s.token {
			c.JSON(http.StatusUnauthorized, gin.H{"ok": false, "error": "unauthorized"})
			c.Abort()
			return
		}
		c.Next()
	}
}

func (s *Server) healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":    "ok",
		"llm_active": s.llm != nil && s.llm.IsActive(),
	})
}

func (s *Server) decide(c *gin.Context) {
	var req deception.DecisionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "invalid request: " + err.Error()})
		return
	}

	// Enforce the site budget even when the sensor sent no deadline.
	ctx, cancel := context.WithTimeout(c.Request.Context(), deception.DecisionTimeoutFor(req.CallSite))
	defer cancel()

	started := time.Now()
	resp := deception.Decide(ctx, s.llm, req)

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
