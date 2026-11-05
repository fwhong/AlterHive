package main

import (
	"github.com/alterhive/alterhive/api"
	"github.com/alterhive/alterhive/internal/deception"
	"github.com/alterhive/alterhive/internal/ipc"
	"github.com/alterhive/alterhive/internal/protocols/strategies/SSH"
	"github.com/alterhive/alterhive/internal/sandbox"
	"github.com/alterhive/alterhive/internal/session"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// sensor is Unix-only: no provider config, no bearer token, no Internet sockets.
func runSensor() {
	configDir := resolveConfigDir()
	if err := sandbox.CheckSensorEnvironment(configDir); err != nil {
		log.Fatal(err)
	}
	if err := sandbox.LockNetwork(); err != nil {
		log.Fatalf("cannot enforce sensor network isolation: %v", err)
	}
	topology, safety := loadTopology(configDir)
	tr := newTracer()
	decider, err := deception.NewRemoteDecisionClient(getEnv("ALTERHIVE_DECISION_SOCKET", "/run/alterhive/decisions/decision.sock"))
	if err != nil {
		log.Fatal(err)
	}
	defer decider.Close()
	mgr := session.NewManager(topology, safety, tr, "staging-web-01", decider)
	uid := envUID("ALTERHIVE_INGRESS_UID", 10003)
	sshListener, err := ipc.Listen(getEnv("ALTERHIVE_SSH_SOCKET", "/run/alterhive/ingress/ssh.sock"), uid)
	if err != nil {
		log.Fatal(err)
	}
	defer sshListener.Close()
	// PROXY header is accepted ONLY after authenticating the Unix peer UID.
	sshListener = ipc.ProxyListener(sshListener)
	strategy := &SSH.SSHStrategy{Manager: mgr}
	if err := strategy.Serve(loadSSHConfig(configDir), tr, sshListener); err != nil {
		log.Fatal(err)
	}
	apiListener, err := ipc.Listen(getEnv("ALTERHIVE_API_SOCKET", "/run/alterhive/ingress/api.sock"), uid)
	if err != nil {
		log.Fatal(err)
	}
	defer apiListener.Close()
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())
	api.NewLocalServer(mgr).RegisterRoutes(router)
	serveHTTP(apiListener, router)
	log.Info("sensor started: AF_UNIX only, no credentials, no TCP/UDP sockets")
	waitForShutdown()
}
