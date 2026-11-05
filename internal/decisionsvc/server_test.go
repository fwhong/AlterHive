package decisionsvc

import (
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func requestDecision(s *Server, body string) *httptest.ResponseRecorder {
	r := gin.New()
	s.RegisterRoutes(r)
	w := httptest.NewRecorder()
	q := httptest.NewRequest("POST", "/v1/decide", strings.NewReader(body))
	q.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, q)
	return w
}
func TestDecisionServiceLimitsAndNoAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Run("body", func(t *testing.T) {
		s := NewServer(nil)
		if w := requestDecision(s, `{"command":"`+strings.Repeat("x", 257<<10)+`"}`); w.Code != 400 {
			t.Fatal(w.Code)
		}
	})
	t.Run("concurrency", func(t *testing.T) {
		s := NewServer(nil)
		for i := 0; i < cap(s.slots); i++ {
			s.slots <- struct{}{}
		}
		if w := requestDecision(s, `{}`); w.Code != 429 {
			t.Fatal(w.Code)
		}
	})
	t.Run("rate", func(t *testing.T) {
		s := NewServer(nil)
		s.burst = 8
		if w := requestDecision(s, `{}`); w.Code != 429 {
			t.Fatal(w.Code)
		}
		s.window = time.Now().Add(-time.Second)
		if w := requestDecision(s, `{}`); w.Code != 200 {
			t.Fatal(w.Code)
		}
	})
	t.Run("daily", func(t *testing.T) {
		s := NewServer(nil)
		s.dailyLimit = 1
		requestDecision(s, `{}`)
		if w := requestDecision(s, `{}`); w.Code != 429 {
			t.Fatal(w.Code)
		}
	})
	t.Run("redaction", func(t *testing.T) {
		s := NewServer(nil)
		w := requestDecision(s, `{"call_site":"unknown"}`)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "decision unavailable") {
			t.Fatal(w.Body.String())
		}
	})
	t.Run("separate-plane", func(t *testing.T) {
		r := gin.New()
		NewServer(nil).RegisterRoutes(r)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/llm/providers", nil))
		if w.Code != 404 {
			t.Fatal(w.Code)
		}
	})
}
