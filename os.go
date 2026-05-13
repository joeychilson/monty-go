package monty

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/joeychilson/monty/internal/ffi"
)

// OSRequest describes a Python operation that needs host OS cooperation.
type OSRequest struct {
	// Function is the Python-visible operation, such as "Path.read_text".
	Function string
	// Args are positional Python arguments valid for the duration of the call.
	Args []Value
	// Kwargs are keyword Python arguments valid for the duration of the call.
	Kwargs []Pair
	// CallID identifies the call when resolving async futures.
	CallID uint32
}

// OSHandler handles Python OS calls paused by Monty.
//
// Return an *Error to raise a specific Python exception. Other errors are
// raised as RuntimeError.
type OSHandler func(ctx context.Context, request OSRequest) (Value, error)

// MountMode controls what host filesystem operations a Mount permits.
type MountMode int

const (
	// MountReadOnly allows reads and rejects writes with PermissionError.
	MountReadOnly MountMode = iota
	// MountReadWrite allows reads and writes through to the host directory.
	MountReadWrite
	// MountOverlay stores writes in memory while reads fall through to the host directory.
	MountOverlay
)

// String returns a stable display name for mode.
func (mode MountMode) String() string {
	switch mode {
	case MountReadOnly:
		return "read-only"
	case MountReadWrite:
		return "read-write"
	case MountOverlay:
		return "overlay"
	default:
		return fmt.Sprintf("MountMode(%d)", int(mode))
	}
}

// Mount maps a virtual Python path prefix to a host directory for one run.
type Mount struct {
	// VirtualPath is the POSIX path prefix visible to Python.
	VirtualPath string
	// HostPath is the host directory backing the virtual path.
	HostPath string
	// Mode controls whether Python may write through this mount.
	Mode MountMode
	// WriteBytesLimit caps cumulative bytes written through this mount when set.
	WriteBytesLimit    uint64
	hasWriteBytesLimit bool
}

// MountOption configures a filesystem mount.
type MountOption func(*Mount)

// WithMountMode sets the mount access policy. NewMountDir defaults to overlay mode.
func WithMountMode(mode MountMode) MountOption {
	return func(m *Mount) { m.Mode = mode }
}

// WithWriteBytesLimit caps cumulative bytes written through the mount.
func WithWriteBytesLimit(limit uint64) MountOption {
	return func(m *Mount) {
		m.WriteBytesLimit = limit
		m.hasWriteBytesLimit = true
	}
}

// WithMount adds a filesystem mount for Python path operations during this run.
func WithMount(virtualPath, hostPath string, mode MountMode) RunOption {
	return func(c *runConfig) {
		c.mounts = append(c.mounts, newMount(virtualPath, hostPath, mode))
	}
}

// WithMountOptions adds a filesystem mount using Go-style options.
func WithMountOptions(virtualPath, hostPath string, opts ...MountOption) RunOption {
	return func(c *runConfig) {
		mount := newMount(virtualPath, hostPath, MountOverlay)
		for _, opt := range opts {
			opt(&mount)
		}
		c.mounts = append(c.mounts, mount)
	}
}

// MountDir is a reusable filesystem mount with persistent overlay state.
//
// Use Close when the mount is no longer needed.
type MountDir struct {
	mu     sync.Mutex
	mount  Mount
	handle uintptr
}

// NewMountDir creates a reusable filesystem mount. It defaults to overlay mode.
func NewMountDir(virtualPath, hostPath string, opts ...MountOption) (*MountDir, error) {
	mount := newMount(virtualPath, hostPath, MountOverlay)
	for _, opt := range opts {
		opt(&mount)
	}
	handle, err := mount.openHandle()
	if err != nil {
		return nil, err
	}
	dir := &MountDir{mount: mount, handle: handle}
	runtime.SetFinalizer(dir, (*MountDir).finalize)
	return dir, nil
}

func (m *MountDir) finalize() {
	m.Close() //nolint:errcheck,gosec // Close cannot return an error and a finalizer cannot propagate one
}

// Close releases the Rust-side mount handle.
func (m *MountDir) Close() error {
	if m == nil {
		return nil
	}
	runtime.SetFinalizer(m, nil)
	m.mu.Lock()
	handle := m.handle
	m.handle = 0
	m.mu.Unlock()
	ffi.MountFree(handle)
	return nil
}

// VirtualPath returns the POSIX path prefix visible to Python.
func (m *MountDir) VirtualPath() string {
	if m == nil {
		return ""
	}
	return m.mount.VirtualPath
}

// HostPath returns the host directory backing the mount.
func (m *MountDir) HostPath() string {
	if m == nil {
		return ""
	}
	return m.mount.HostPath
}

// Mode returns the mount access policy.
func (m *MountDir) Mode() MountMode {
	if m == nil {
		return MountReadOnly
	}
	return m.mount.Mode
}

func (m *MountDir) ffiHandle() (uintptr, error) {
	if m == nil {
		return 0, fmt.Errorf("monty: mount is nil")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.handle == 0 {
		return 0, fmt.Errorf("monty: mount is closed")
	}
	return m.handle, nil
}

// WithMountDir uses a reusable filesystem mount during this run.
func WithMountDir(mount *MountDir) RunOption {
	return func(c *runConfig) {
		if mount != nil {
			c.mountDirs = append(c.mountDirs, mount)
		}
	}
}

func newMount(virtualPath, hostPath string, mode MountMode) Mount {
	if virtualPath == "" {
		virtualPath = "/"
	} else if !strings.HasPrefix(virtualPath, "/") {
		virtualPath = "/" + virtualPath
	}
	return Mount{
		VirtualPath: path.Clean(virtualPath),
		HostPath:    filepath.Clean(hostPath),
		Mode:        mode,
	}
}

func (c runConfig) needsDispatchLoop() bool {
	return len(c.functions) != 0 || c.osHandler != nil || len(c.mounts) != 0 || len(c.mountDirs) != 0
}

func (c runConfig) canUseFunctionCallbackFast() bool {
	if len(c.functions) == 0 || c.osHandler != nil || len(c.mounts) != 0 || len(c.mountDirs) != 0 {
		return false
	}
	for _, function := range c.functions {
		if function == nil || function.fastRawCall == nil {
			return false
		}
	}
	return true
}

func (c runConfig) handleOSCall(ctx context.Context, call *OSCall) (Value, error) {
	if value, handled, err := c.handleMount(call); handled {
		return value, err
	}
	if c.osHandler != nil {
		return c.osHandler(ctx, OSRequest{
			Function: call.Function,
			Args:     call.Args,
			Kwargs:   call.Kwargs,
			CallID:   call.CallID,
		})
	}
	return Value{}, &Error{
		Type:    "PermissionError",
		Message: fmt.Sprintf("%s is not permitted", call.Function),
	}
}

func (c runConfig) handleMount(call *OSCall) (result Value, handled bool, err error) {
	if len(c.mountHandles) == 0 || !isPathFunction(call.Function) || len(call.Args) == 0 {
		return Value{}, false, nil
	}
	argHandles, err := valuesToHandles(call.Args)
	if err != nil {
		return Value{}, true, err
	}
	defer freeHandles(argHandles)
	kwargKeyHandles, kwargValueHandles, err := pairsToHandles(call.Kwargs)
	if err != nil {
		return Value{}, true, err
	}
	defer freeHandles(kwargKeyHandles)
	defer freeHandles(kwargValueHandles)

	valueHandle, handled, err := ffi.MountHandleOSCall(
		c.mountHandles,
		call.Function,
		argHandles,
		kwargKeyHandles,
		kwargValueHandles,
	)
	if err != nil {
		return Value{}, true, normalizeError(err)
	}
	if !handled {
		return Value{}, false, nil
	}
	value, err := decodeOwnedValue(valueHandle)
	return value, true, normalizeError(err)
}

func (c *runConfig) openMounts() error {
	if len(c.mounts) == 0 && len(c.mountDirs) == 0 {
		return nil
	}
	c.mountHandles = make([]uintptr, 0, len(c.mounts)+len(c.mountDirs))
	c.ownedMounts = make([]uintptr, 0, len(c.mounts))
	for _, mount := range c.mounts {
		handle, err := mount.openHandle()
		if err != nil {
			c.closeMounts()
			return err
		}
		c.mountHandles = append(c.mountHandles, handle)
		c.ownedMounts = append(c.ownedMounts, handle)
	}
	for _, mount := range c.mountDirs {
		handle, err := mount.ffiHandle()
		if err != nil {
			c.closeMounts()
			return err
		}
		c.mountHandles = append(c.mountHandles, handle)
	}
	return nil
}

func (c *runConfig) closeMounts() {
	for _, handle := range c.ownedMounts {
		ffi.MountFree(handle)
	}
	c.ownedMounts = nil
	c.mountHandles = nil
}

func (mount Mount) openHandle() (uintptr, error) {
	var limit *uint64
	if mount.hasWriteBytesLimit {
		limit = &mount.WriteBytesLimit
	}
	//nolint:gosec // MountMode is a small enum (read-only, overlay, read-write)
	handle, err := ffi.MountNew(mount.VirtualPath, mount.HostPath, uint32(mount.Mode), limit)
	return handle, normalizeError(err)
}

func valuesToHandles(values []Value) ([]uintptr, error) {
	handles := make([]uintptr, len(values))
	for i, value := range values {
		handle, err := valueToHandle(value)
		if err != nil {
			freeHandles(handles)
			return nil, err
		}
		handles[i] = handle
	}
	return handles, nil
}

func pairsToHandles(pairs []Pair) ([]uintptr, []uintptr, error) {
	keyHandles := make([]uintptr, len(pairs))
	valueHandles := make([]uintptr, len(pairs))
	for i := range pairs {
		key, err := valueToHandle(pairs[i].Key)
		if err != nil {
			freeHandles(keyHandles)
			freeHandles(valueHandles)
			return nil, nil, err
		}
		keyHandles[i] = key
		value, err := valueToHandle(pairs[i].Value)
		if err != nil {
			freeHandles(keyHandles)
			freeHandles(valueHandles)
			return nil, nil, err
		}
		valueHandles[i] = value
	}
	return keyHandles, valueHandles, nil
}

func isPathFunction(function string) bool {
	switch function {
	case "Path.exists", "Path.is_file", "Path.is_dir", "Path.is_symlink",
		"Path.read_text", "Path.read_bytes", "Path.write_text", "Path.write_bytes",
		"Path.mkdir", "Path.unlink", "Path.rmdir", "Path.iterdir", "Path.stat",
		"Path.rename", "Path.resolve", "Path.absolute":
		return true
	default:
		return false
	}
}
