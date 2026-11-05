package sandbox

import (
	"fmt"
	"golang.org/x/sys/unix"
	"runtime"
	"unsafe"
)

// LockNetwork is irreversible and applies to ALL Go runtime threads (TSYNC),
// future threads and exec children. Only AF_UNIX sockets remain available.
// Docker network_mode:none separately covers exec processes not descended from AH.
func LockNetwork() error {
	var arch uint32
	switch runtime.GOARCH {
	case "arm64":
		arch = unix.AUDIT_ARCH_AARCH64
	case "amd64":
		arch = unix.AUDIT_ARCH_X86_64
	default:
		return fmt.Errorf("unsupported seccomp architecture")
	}
	stmt := func(code uint16, k uint32) unix.SockFilter { return unix.SockFilter{Code: code, K: k} }
	jump := func(k uint32, t, f uint8) unix.SockFilter {
		return unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: k, Jt: t, Jf: f}
	}
	deny := stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ERRNO|uint32(unix.EPERM))
	filter := []unix.SockFilter{
		stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, 4), jump(arch, 1, 0), stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_KILL_PROCESS),
		stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, 0),
	}
	// x32 shares AUDIT_ARCH_X86_64 but has a different syscall ABI.
	if runtime.GOARCH == "amd64" {
		filter = append(filter, unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K, K: 0x40000000, Jt: 0, Jf: 1}, deny)
	}
	for _, nr := range []uint32{unix.SYS_IO_URING_SETUP, unix.SYS_PTRACE, unix.SYS_PROCESS_VM_WRITEV, unix.SYS_BPF} {
		filter = append(filter, jump(nr, 0, 1), deny)
	}
	// Keep nr in accumulator until a matching socket syscall; args[0] is at 16.
	for _, nr := range []uint32{unix.SYS_SOCKET, unix.SYS_SOCKETPAIR} {
		filter = append(filter, jump(nr, 0, 4), stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, 16), jump(unix.AF_UNIX, 1, 0), deny, stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW))
	}
	filter = append(filter, stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW))
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if e := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); e != nil {
		return e
	}
	r, _, e := unix.RawSyscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&prog)))
	runtime.KeepAlive(filter)
	if e != 0 {
		return e
	}
	if r != 0 {
		return fmt.Errorf("seccomp thread synchronization failed: %d", r)
	}
	return nil
}
