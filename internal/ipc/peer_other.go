//go:build !linux && !darwin

package ipc

import (
	"fmt"
	"net"
)

func PeerUID(*net.UnixConn) (uint32, error) {
	return 0, fmt.Errorf("peer authentication unsupported on this OS")
}
