// Direct entry points to purego's underlying syscall trampoline. Calling
// purego.SyscallN via its public variadic signature forces the args slice
// onto the heap on every FFI hop (purego marks SyscallN with
// //go:uintptrescapes). Linking to the unexported syscall_syscall15X — which
// takes fixed uintptr parameters — skips that escape so a typical Monty FFI
// call allocates nothing on the Go side beyond the result. This is the same
// pattern Go's standard library uses for syscall.Syscall* (see syscall_unix.go).
//
//go:build darwin || (linux && (amd64 || arm64))

package ffi

import (
	_ "unsafe" // for go:linkname

	_ "github.com/ebitengine/purego" // for go:linkname target
)

// syscall_syscall15X must match purego's unexported function name so go:linkname
// resolves correctly; the underscore is required and not a style choice.
//
//nolint:revive // name must match upstream go:linkname target
//go:linkname syscall_syscall15X github.com/ebitengine/purego.syscall_syscall15X
func syscall_syscall15X(fn, a1, a2, a3, a4, a5, a6, a7, a8, a9, a10, a11, a12, a13, a14, a15 uintptr) (r1, r2, err uintptr)

// syscall5 calls a C function via purego's trampoline with five uintptr
// arguments. Used by the hot FFI paths where five args is enough.
func syscall5(fn, a1, a2, a3, a4, a5 uintptr) uintptr {
	r, _, _ := syscall_syscall15X(fn, a1, a2, a3, a4, a5, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	return r
}

// syscall2 calls a C function via purego's trampoline with two uintptr
// arguments. Used for short helpers like mg_bytes_free.
func syscall2(fn, a1, a2 uintptr) uintptr {
	r, _, _ := syscall_syscall15X(fn, a1, a2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	return r
}

// syscall3 calls a C function via purego's trampoline with three uintptr
// arguments. Used by progress resume helpers that pass handle, value, and out.
func syscall3(fn, a1, a2, a3 uintptr) uintptr {
	r, _, _ := syscall_syscall15X(fn, a1, a2, a3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	return r
}

// syscall4 calls a C function via purego's trampoline with four uintptr
// arguments. Used by ProgressResumeFutures.
func syscall4(fn, a1, a2, a3, a4 uintptr) uintptr {
	r, _, _ := syscall_syscall15X(fn, a1, a2, a3, a4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	return r
}

// syscall6 calls a C function via purego's trampoline with six uintptr
// arguments. Used by ProgressResumeException.
func syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) uintptr {
	r, _, _ := syscall_syscall15X(fn, a1, a2, a3, a4, a5, a6, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	return r
}
