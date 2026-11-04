package main

import (
	"net/http"
	"os"

	"github.com/alterhive/alterhive/api"
	"github.com/alterhive/alterhive/internal/decisionsvc"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// runManager is the central decision role. It holds the LLM providers and
// credentials (configs/llm.yaml + ALTERHIVE_*_API_KEY env), serves sensor
// decisions on ALTERHIVE_MANAGER_ADDR and exposes the LLM admin API. It runs
// no SSH listener and no session state.
func runManager() {
	token := os.Getenv("ALTERHIVE_MANAGER_TOKEN")
	if token == "" {
		log.Fatal("ALTERHIVE_MANAGER_TOKEN is required in manager role (refusing to start with no auth)")
	}

	configDir := resolveConfigDir()
	llmMgr := loadLocalLLM(configDir)

	addr := getEnv("ALTERHIVE_MANAGER_ADDR", ":8100")

	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())
	router.Use(corsMiddleware())

	// Decision service (sensor→manager cold path).
	svc := decisionsvc.NewServer(llmMgr, token)
	svc.RegisterRoutes(router)

	// LLM administration (ops-facing) reusing the api package handlers.
	admin := api.NewServer(nil, llmMgr)
	admin.RegisterLLMRoutes(router.Group("/api/v1"))

	log.WithFields(log.Fields{
		"role":      RoleManager,
		"addr":      addr,
		"llm_active": llmMgr.IsActive(),
	}).Info("manager started: holding LLM credentials, serving decisions")

	if err := http.ListenAndServe(addr, router); err != nil && err != http.ErrServerClosed {
		log.Fatalf("manager server error: %v", err)
	}
}
