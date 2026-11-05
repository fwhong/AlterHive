package deception

import (
	"os"
	"testing"
)

// Tests using synthetic completion clients opt into context export explicitly.
func TestMain(m *testing.M) {
	os.Setenv("ALTERHIVE_ALLOW_LLM_CONTEXT_EXPORT", "true")
	os.Exit(m.Run())
}
