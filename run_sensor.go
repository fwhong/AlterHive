package main

import (
	"os"

	"github.com/alterhive/alterhive/internal/deception"
	"github.com/alterhive/alterhive/internal/llm"
	"github.com/alterhive/alterhive/internal/session"

	log "github.com/sirupsen/logrus"
)

// runSensor is the honeypot-only role. The process never reads llm.yaml and
// holds no provider credentials; cold-path decisions are forwarded to the
// central manager over ALTERHIVE_MANAGER_URL. When the manager is
// unreachable, the hot path keeps serving deterministic responses exactly
// like an LLM-disabled standalone deployment.
func runSensor() {
	managerURL := getEnv("ALTERHIVE_MANAGER_URL", "")
	managerToken := os.Getenv("ALTERHIVE_MANAGER_TOKEN")
	if managerURL == "" {
		log.Fatal("ALTERHIVE_MANAGER_URL is required in sensor role")
	}

	configDir := resolveConfigDir()
	topology, safety := loadTopology(configDir)
	tr := newTracer()

	// No llm.yaml is loaded here by design: the sensor's API server gets an
	// empty (inactive, credential-free) LLM manager so the LLM admin routes
	// report "not configured" instead of leaking manager-side state.
	emptyLLM := llm.NewManager(&llm.LLMConfig{Providers: []llm.ProviderConfig{}}, "")

	decider := deception.NewRemoteDecisionClient(managerURL, managerToken)
	mgr := session.NewManager(topology, safety, tr, "staging-web-01", decider)

	sshConf := loadSSHConfig(configDir)
	startSSH(mgr, sshConf, tr)

	startHoneypotAPI(mgr, emptyLLM, getEnv("API_ADDR", ":8000"))

	log.WithFields(log.Fields{
		"role":    RoleSensor,
		"manager": managerURL,
	}).Info("sensor started: cold path forwarded to manager")

	waitForShutdown()
}
