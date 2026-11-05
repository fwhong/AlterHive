package sandbox

import (
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"runtime"
	"testing"
)

func TestNetworkFilter(t *testing.T) {
	if os.Getenv("AH_SANDBOX_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestNetworkFilter$")
		cmd.Env = append(os.Environ(), "AH_SANDBOX_CHILD=1")
		if out, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("%v\n%s", e, out)
		}
		return
	}
	if e := LockNetwork(); e != nil {
		t.Fatal(e)
	}
	for _, family := range []int{unix.AF_INET, unix.AF_INET6, unix.AF_PACKET, unix.AF_NETLINK} {
		for _, kind := range []int{unix.SOCK_STREAM, unix.SOCK_DGRAM} {
			fd, e := unix.Socket(family, kind, 0)
			if e == nil {
				unix.Close(fd)
				t.Fatalf("network socket allowed: %d/%d", family, kind)
			}
		}
	}
	fd, e := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if e != nil {
		t.Fatal(e)
	}
	unix.Close(fd)
	// Keep this thread occupied so the worker must run on another OS thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// TSYNC also covers work scheduled on a different runtime thread.
	done := make(chan error, 1)
	go func() {
		fd, e := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
		if e == nil {
			unix.Close(fd)
		}
		done <- e
	}()
	if <-done == nil {
		t.Fatal("worker thread escaped filter")
	}
}
