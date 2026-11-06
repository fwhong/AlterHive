package engine

import (
	"github.com/alterhive/alterhive/internal/domain"
	"regexp"
	"strings"
)

var openScanPort = regexp.MustCompile(`(?m)\b(\d+/(?:tcp|udp))\s+open(?:\|filtered)?\b`)

// A short/non-empty answer is not enough: the authoritative Nmap baseline's
// hosts and open ports must survive enrichment. Otherwise retain the rule
// response and do not poison the per-session cache with partial model output.
func preservesScanFacts(command, baseline, candidate string) bool {
	if command != "nmap" {
		return true
	}
	present := map[string]bool{}
	for _, ip := range domain.ExtractIPsFromCommand(candidate) {
		present[ip] = true
	}
	for _, ip := range domain.ExtractIPsFromCommand(baseline) {
		if !present[ip] {
			return false
		}
	}
	ports := map[string]bool{}
	for _, match := range openScanPort.FindAllStringSubmatch(strings.ToLower(candidate), -1) {
		ports[match[1]] = true
	}
	for _, match := range openScanPort.FindAllStringSubmatch(strings.ToLower(baseline), -1) {
		if !ports[match[1]] {
			return false
		}
	}
	return true
}
