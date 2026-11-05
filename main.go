package main

import (
	"fmt"
	"os"
)

const banner = "\n  AlterHive v3.0 - Honeypot for Adversarial AI Agents (Go)\n\n"

// ALTERHIVE_ROLE selects the process role:
//
//	standalone (default) — single-process honeypot: local LLM decisions, no split
//	sensor               — honeypot without LLM credentials; cold-path decisions
//	                       are forwarded to the central manager
//	ingress              — TCP front door forwarding only to sensor Unix sockets
//	manager              — central decision service: holds LLM providers/keys,
//	                       answers sensor decisions, exposes LLM admin API
const (
	RoleStandalone = "standalone"
	RoleSensor     = "sensor"
	RoleManager    = "manager"
	RoleIngress    = "ingress"
)

func main() {
	fmt.Print(banner)

	role := getEnv("ALTERHIVE_ROLE", RoleStandalone)

	switch role {
	case RoleManager:
		runManager()
	case RoleSensor:
		runSensor()
	case RoleIngress:
		runIngress()
	case RoleStandalone:
		runStandalone()
	default:
		fmt.Fprintln(os.Stderr, "invalid ALTERHIVE_ROLE")
		os.Exit(1)
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
