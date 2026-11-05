package main

import (
	"github.com/alterhive/alterhive/api"
	"github.com/alterhive/alterhive/internal/decisionsvc"
	"github.com/alterhive/alterhive/internal/ipc"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

// Decision IPC and ops HTTP have disjoint routers, listeners and credentials.
func runManager() {
	if os.Getenv("ALTERHIVE_MANAGER_TOKEN") != "" || os.Getenv("ALTERHIVE_MANAGER_ADDR") != "" {
		log.Fatal("legacy TCP decision configuration is not supported")
	}
	llmMgr := loadLocalLLM(resolveConfigDir())
	readonly, err := strconv.ParseBool(getEnv("ALTERHIVE_LLM_CONFIG_READ_ONLY", "true"))
	if err != nil {
		log.Fatal("invalid ALTERHIVE_LLM_CONFIG_READ_ONLY")
	}
	llmMgr.SetReadOnly(readonly)
	listener, err := ipc.Listen(getEnv("ALTERHIVE_DECISION_SOCKET", "/run/alterhive/decisions/decision.sock"), envUID("ALTERHIVE_SENSOR_UID", 10002))
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()
	gin.SetMode(gin.ReleaseMode)
	decisions := gin.New()
	decisions.Use(gin.Recovery())
	decisionsvc.NewServer(llmMgr).RegisterRoutes(decisions)
	serveHTTP(listener, decisions)
	// No admin listener at all unless explicitly configured with a strong token.
	if addr := os.Getenv("ALTERHIVE_ADMIN_ADDR"); addr != "" {
		requireAdminToken()
		ops := gin.New()
		ops.Use(gin.Recovery(), corsMiddleware())
		admin := api.NewServer(nil, llmMgr)
		v1 := ops.Group("/api/v1")
		admin.RegisterAuthRoutes(v1)
		admin.RegisterLLMRoutes(v1)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			log.Fatal(err)
		}
		defer ln.Close()
		serveHTTP(ln, ops)
	}
	log.Info("manager started: UID-authenticated Unix decisions; separate ops listener")
	waitForShutdown()
}
func envUID(name string, fallback uint32) uint32 {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, e := strconv.ParseUint(v, 10, 32)
	if e != nil {
		log.Fatalf("invalid %s", name)
	}
	return uint32(n)
}
func requireAdminToken() {
	if len(os.Getenv("ALTERHIVE_ADMIN_TOKEN")) < 32 {
		log.Fatal("ALTERHIVE_ADMIN_TOKEN must contain at least 32 characters")
	}
}
func serveHTTP(ln net.Listener, handler http.Handler) *http.Server {
	bounded := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
		handler.ServeHTTP(w, r)
	})
	s := &http.Server{Handler: bounded, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 65 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() {
		if err := s.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.WithError(err).Fatal("HTTP server stopped")
		}
	}()
	return s
}
