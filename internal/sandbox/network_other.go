//go:build !linux

package sandbox

import "fmt"

func LockNetwork() error {
	return fmt.Errorf("sensor requires Linux seccomp; use the Docker deployment")
}
