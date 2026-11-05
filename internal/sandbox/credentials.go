package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The sensor accepts an explicit deployment allowlist, not heuristics for known
// provider names: custom key_env names must not silently inherit any secret.
func CheckSensorEnvironment(configDir string) error {
	allowed := map[string]bool{}
	for _, k := range strings.Fields("PATH HOSTNAME HOME LANG LC_ALL TZ TERM TMPDIR ALTERHIVE_ROLE CONFIG_DIR ALTERHIVE_DECISION_SOCKET ALTERHIVE_SSH_SOCKET ALTERHIVE_API_SOCKET ALTERHIVE_INGRESS_UID ALTERHIVE_ALLOW_LLM_CONTEXT_EXPORT SESSION_TIMEOUT TOPOLOGY_CIDR ENTRY_IP ENTRY_CIDR ENTRY_GATEWAY TOPOLOGY_ENTRY_IP TOPOLOGY_ENTRY_CIDR TOPOLOGY_ENTRY_GATEWAY") {
		allowed[k] = true
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !allowed[key] {
			return fmt.Errorf("sensor refuses unexpected environment variable %s", key)
		}
	}
	if _, err := os.Lstat(filepath.Join(configDir, "llm.yaml")); err == nil {
		return fmt.Errorf("sensor must not mount llm.yaml")
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}
