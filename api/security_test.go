package api

import (
	"bytes"
	"github.com/alterhive/alterhive/internal/llm"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func call(r http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	r.ServeHTTP(w, req)
	return w
}
func TestNoDefaultAdminAndNoLegacyLogin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("ALTERHIVE_ADMIN_TOKEN", "")
	r := gin.New()
	s := NewServer(nil, nil)
	s.RegisterAuthRoutes(r.Group("/api/v1"))
	if call(r, "GET", "/api/v1/auth/me", "", "alterhive-admin-token").Code != 401 {
		t.Fatal("default token accepted")
	}
	t.Setenv("ALTERHIVE_ADMIN_TOKEN", strings.Repeat("x", 40))
	r = gin.New()
	NewServer(nil, nil).RegisterAuthRoutes(r.Group("/api/v1"))
	if call(r, "POST", "/api/v1/auth/login", `{"username":"admin","password":"admin"}`, "").Code != 401 {
		t.Fatal("default password accepted")
	}
	if call(r, "GET", "/api/v1/auth/me", "", strings.Repeat("x", 40)).Code != 200 {
		t.Fatal("configured token rejected")
	}
}
func TestSensorHasNoProviderOrLoginRoutes(t *testing.T) {
	r := gin.New()
	NewLocalServer(nil).RegisterRoutes(r)
	for _, tc := range [][2]string{{"GET", "/api/v1/llm/providers"}, {"PUT", "/api/v1/llm/providers/mock"}, {"POST", "/api/v1/auth/login"}, {"PATCH", "/api/v1/config/runtime"}} {
		if call(r, tc[0], tc[1], `{"api_key":"SYNTHETIC"}`, "").Code != 404 {
			t.Fatalf("sensor exposes %v", tc)
		}
	}
}
func TestAdminResponseRedactsProviderKey(t *testing.T) {
	token := strings.Repeat("a", 40)
	t.Setenv("ALTERHIVE_ADMIN_TOKEN", token)
	m := llm.NewManager(&llm.LLMConfig{Providers: []llm.ProviderConfig{{ID: "mock", APIKey: "SYNTHETIC-SECRET"}}}, "")
	r := gin.New()
	NewServer(nil, m).RegisterLLMRoutes(r.Group("/api/v1"))
	for _, path := range []string{"/api/v1/llm/providers", "/api/v1/llm/active"} {
		w := call(r, "GET", path, "", token)
		if w.Code != 200 || strings.Contains(w.Body.String(), "SECRET") {
			t.Fatalf("unsafe response: %s", w.Body)
		}
	}
}

func TestStandaloneSimulationRequiresAuthentication(t *testing.T) {
	t.Setenv("ALTERHIVE_ADMIN_TOKEN", strings.Repeat("a", 40))
	r := gin.New()
	NewServer(nil, nil).RegisterRoutes(r)
	for _, path := range []string{"/api/v1/simulate/nmap", "/api/v1/simulate/goal"} {
		if call(r, "POST", path, `{"target":"192.168.56.60"}`, "").Code != 401 {
			t.Fatal("anonymous simulation exposed")
		}
	}
}
