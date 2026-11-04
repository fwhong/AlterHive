package main

import (
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/alterhive/alterhive/api"
	"github.com/alterhive/alterhive/internal/deception"
	"github.com/alterhive/alterhive/internal/domain"
	"github.com/alterhive/alterhive/internal/llm"
	"github.com/alterhive/alterhive/internal/parser"
	"github.com/alterhive/alterhive/internal/protocols/strategies/SSH"
	"github.com/alterhive/alterhive/internal/session"
	"github.com/alterhive/alterhive/internal/tracer"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// runStandalone is the upstream single-process behavior: the honeypot loads
// its own LLM config and wraps it into a local decision client, so the
// decision path is identical to the split deployment by construction.
func runStandalone() {
	configDir := resolveConfigDir()

	topology, safety := loadTopology(configDir)
	tr := newTracer()

	// --- Load LLM config ---
	llmMgr := loadLocalLLM(configDir)

	// --- Session manager (local decision client) ---
	decider := deception.AsDecisionClient(llmMgr)
	mgr := session.NewManager(topology, safety, tr, "staging-web-01", decider)

	sshConf := loadSSHConfig(configDir)
	startSSH(mgr, sshConf, tr)

	startHoneypotAPI(mgr, llmMgr, getEnv("API_ADDR", ":8000"))
	waitForShutdown()
}

// loadLocalLLM loads configs/llm.yaml when present; a missing config keeps
// the LLM disabled (rules-only mode).
func loadLocalLLM(configDir string) *llm.Manager {
	llmConfigPath := filepath.Join(configDir, "llm.yaml")
	llmConfig, err := llm.LoadLLMConfig(llmConfigPath)
	if err != nil {
		log.WithError(err).Warn("Failed to load LLM config, LLM features disabled")
		llmConfig = &llm.LLMConfig{Providers: []llm.ProviderConfig{}}
	}
	llmMgr := llm.NewManager(llmConfig, llmConfigPath)

	if llmMgr.IsActive() {
		log.WithField("provider", llmConfig.ActiveProvider).Info("LLM active")
	} else {
		log.Info("LLM disabled (no provider configured)")
	}
	return llmMgr
}

func resolveConfigDir() string {
	configDir := getEnv("CONFIG_DIR", "configs")
	if _, err := os.Stat(configDir); os.IsNotExist(err) {
		configDir = "/app/configs" // Docker fallback
	}
	return configDir
}

func loadTopology(configDir string) (*domain.VirtualTopology, *domain.SafetyPolicy) {
	topologyData, err := os.ReadFile(filepath.Join(configDir, "topology.yaml"))
	if err != nil {
		log.Fatalf("Failed to read topology config: %v", err)
	}
	topologyConfig, err := domain.LoadTopologyConfig(topologyData)
	if err != nil {
		log.Fatalf("Failed to parse topology config: %v", err)
	}

	if v := os.Getenv("TOPOLOGY_CIDR"); v != "" {
		topologyConfig, err = domain.RebasePrimaryCIDR(topologyConfig, v)
		if err != nil {
			log.Fatalf("Failed to rebase topology CIDR: %v", err)
		}
	}

	topology := domain.NewVirtualTopology(topologyConfig)
	safety := domain.NewSafetyPolicy(topologyConfig.CIDR)
	for _, segment := range topology.AllSegments() {
		safety.AllowCIDR(segment.CIDR)
	}

	log.WithFields(log.Fields{
		"cidr":  topologyConfig.CIDR,
		"hosts": len(topologyConfig.Hosts),
	}).Info("Topology loaded")
	return topology, safety
}

func newTracer() tracer.Tracer {
	return tracer.GetInstance(func(event tracer.Event) {
		log.WithFields(log.Fields{
			"protocol": event.Protocol,
			"status":   event.Status,
			"command":  event.Command,
			"user":     event.User,
		}).Info("Honeypot Event")
	})
}

func loadSSHConfig(configDir string) parser.BeelzebubServiceConfiguration {
	servicesDir := filepath.Join(configDir, "services")
	p := parser.Init("", servicesDir)
	services, err := p.ReadConfigurationsServices()
	if err != nil {
		log.Fatalf("Failed to read SSH service config: %v", err)
	}
	if len(services) == 0 {
		log.Fatal("No service configurations found in configs/services/")
	}
	sshConf := services[0] // First service config (ssh.yaml)

	if v := os.Getenv("SSH_ADDR"); v != "" {
		sshConf.Address = v
	}
	if v := os.Getenv("SESSION_TIMEOUT"); v != "" {
		if seconds, err := strconv.Atoi(v); err == nil && seconds > 0 {
			sshConf.DeadlineTimeoutSeconds = seconds
		}
	}
	return sshConf
}

func startSSH(mgr *session.Manager, sshConf parser.BeelzebubServiceConfiguration, tr tracer.Tracer) {
	sshStrategy := &SSH.SSHStrategy{Manager: mgr}
	if err := sshStrategy.Init(sshConf, tr); err != nil {
		log.Fatalf("Failed to start SSH: %v", err)
	}
	log.WithField("address", sshConf.Address).Info("SSH honeypot started")
}

func startHoneypotAPI(mgr *session.Manager, llmMgr *llm.Manager, apiAddr string) {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())
	router.Use(corsMiddleware())

	apiServer := api.NewServer(mgr, llmMgr)
	apiServer.RegisterRoutes(router)

	log.WithField("api", apiAddr).Info("API server started")
	go func() {
		if err := http.ListenAndServe(apiAddr, router); err != nil && err != http.ErrServerClosed {
			log.Fatalf("API server error: %v", err)
		}
	}()
}

func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

func waitForShutdown() {
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Info("Shutting down AlterHive...")
}
