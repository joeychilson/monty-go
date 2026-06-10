package monty

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/joeychilson/monty/internal/ffi"
)

// OSFunction names a Python operation that needs host OS cooperation. The
// set is open: unknown literals pass through for forward compatibility.
type OSFunction string

// The OS functions Monty traps.
const (
	OSPathExists     OSFunction = "Path.exists"
	OSPathIsFile     OSFunction = "Path.is_file"
	OSPathIsDir      OSFunction = "Path.is_dir"
	OSPathIsSymlink  OSFunction = "Path.is_symlink"
	OSPathReadText   OSFunction = "Path.read_text"
	OSPathReadBytes  OSFunction = "Path.read_bytes"
	OSPathWriteText  OSFunction = "Path.write_text"
	OSPathWriteBytes OSFunction = "Path.write_bytes"
	OSPathMkdir      OSFunction = "Path.mkdir"
	OSPathUnlink     OSFunction = "Path.unlink"
	OSPathRmdir      OSFunction = "Path.rmdir"
	OSPathIterDir    OSFunction = "Path.iterdir"
	OSPathStat       OSFunction = "Path.stat"
	OSPathRename     OSFunction = "Path.rename"
	OSPathResolve    OSFunction = "Path.resolve"
	OSPathAbsolute   OSFunction = "Path.absolute"
	OSGetenv         OSFunction = "os.getenv"
	OSDateToday      OSFunction = "date.today"
	OSDateTimeNow    OSFunction = "datetime.now"
)

// IsFilesystem reports whether the function operates on paths.
func (fn OSFunction) IsFilesystem() bool {
	switch fn {
	case OSPathExists, OSPathIsFile, OSPathIsDir, OSPathIsSymlink,
		OSPathReadText, OSPathReadBytes, OSPathWriteText, OSPathWriteBytes,
		OSPathMkdir, OSPathUnlink, OSPathRmdir, OSPathIterDir, OSPathStat,
		OSPathRename, OSPathResolve, OSPathAbsolute:
		return true
	default:
		return false
	}
}

// ErrNotHandled is returned by an OSHandler (or fs.FS dispatch) to decline a
// call: Monty's default behavior then applies (PermissionError for filesystem
// operations, RuntimeError otherwise).
var ErrNotHandled = errors.New("monty: os call not handled")

// OSHandler answers Python OS calls — filesystem operations, os.getenv,
// date.today, datetime.now. It runs after mounts and WithFS filesystems
// decline. The returned value converts with the same rules as From; return
// ErrNotHandled to fall through, or a *Exception to raise a specific Python
// exception.
type OSHandler func(ctx context.Context, fn OSFunction, args []Value, kwargs map[string]Value) (any, error)

// StatResult is an os.stat_result payload for answering Path.stat calls.
type StatResult struct {
	Mode  uint32
	Ino   uint64
	Dev   uint64
	Nlink int
	UID   int
	GID   int
	Size  int64
	ATime time.Time
	MTime time.Time
	CTime time.Time
}

// MontyValue implements Valuer, encoding the canonical os.stat_result
// namedtuple.
func (s StatResult) MontyValue() Value {
	toFloat := func(t time.Time) Value {
		if t.IsZero() {
			return Float(0)
		}
		return Float(float64(t.UnixNano()) / float64(time.Second))
	}
	return NamedTuple{
		Type: "os.stat_result",
		Fields: []string{
			"st_mode", "st_ino", "st_dev", "st_nlink", "st_uid", "st_gid",
			"st_size", "st_atime", "st_mtime", "st_ctime",
		},
		Values: []Value{
			//nolint:gosec // stat fields are far below int64 range in practice
			Int64(int64(s.Mode)), Int64(int64(s.Ino)), Int64(int64(s.Dev)),
			Int(s.Nlink), Int(s.UID), Int(s.GID),
			Int64(s.Size), toFloat(s.ATime), toFloat(s.MTime), toFloat(s.CTime),
		},
	}.MontyValue()
}

// FileStat builds a regular-file stat result with mode 0o644.
func FileStat(size int64, modTime time.Time) StatResult {
	return StatResult{
		Mode:  0o100_644,
		Nlink: 1,
		Size:  size,
		ATime: modTime,
		MTime: modTime,
		CTime: modTime,
	}
}

// DirStat builds a directory stat result with mode 0o755.
func DirStat(modTime time.Time) StatResult {
	return StatResult{
		Mode:  0o040_755,
		Nlink: 2,
		Size:  4096,
		ATime: modTime,
		MTime: modTime,
		CTime: modTime,
	}
}

// StatOf adapts an fs.FileInfo into a stat result.
func StatOf(info fs.FileInfo) StatResult {
	if info.IsDir() {
		return DirStat(info.ModTime())
	}
	return FileStat(info.Size(), info.ModTime())
}

// --------------------------------------------------------------------------
// Mounts
// --------------------------------------------------------------------------

// MountMode controls what host filesystem operations a mount permits. The
// zero value is MountOverlay, matching upstream Monty's default.
type MountMode int

const (
	// MountOverlay stores writes in memory while reads fall through to the
	// host directory (copy-on-write).
	MountOverlay MountMode = iota
	// MountReadOnly allows reads and rejects writes with PermissionError.
	MountReadOnly
	// MountReadWrite allows reads and writes through to the host directory.
	MountReadWrite
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

// ffiMode maps the Go enum (overlay-by-default) onto the FFI encoding.
func (mode MountMode) ffiMode() uint32 {
	switch mode {
	case MountReadOnly:
		return 0
	case MountReadWrite:
		return 1
	default:
		return 2
	}
}

// MountOption configures a MountDir.
type MountOption func(*mountConfig)

type mountConfig struct {
	mode          MountMode
	writeLimit    uint64
	hasWriteLimit bool
}

// WithMode sets the mount access policy (the default is MountOverlay).
func WithMode(mode MountMode) MountOption {
	return func(c *mountConfig) { c.mode = mode }
}

// WithWriteLimit caps cumulative bytes written through the mount.
func WithWriteLimit(limit uint64) MountOption {
	return func(c *mountConfig) {
		c.writeLimit = limit
		c.hasWriteLimit = true
	}
}

// MountDir maps a virtual Python path prefix to a host directory. A MountDir
// is reusable across runs; overlay mounts keep their in-memory writes between
// runs. Close it when no longer needed.
type MountDir struct {
	mu          sync.Mutex
	virtualPath string
	hostPath    string
	mode        MountMode
	handle      uintptr
	cleanup     runtime.Cleanup
}

// NewMountDir creates a filesystem mount. The default mode is MountOverlay.
func NewMountDir(virtualPath, hostPath string, opts ...MountOption) (*MountDir, error) {
	config := mountConfig{mode: MountOverlay}
	for _, opt := range opts {
		opt(&config)
	}
	virtualPath = cleanVirtualPath(virtualPath)
	hostPath = filepath.Clean(hostPath)
	var limit *uint64
	if config.hasWriteLimit {
		limit = &config.writeLimit
	}
	handle, err := ffi.MountNew(virtualPath, hostPath, config.mode.ffiMode(), limit)
	if err != nil {
		return nil, normalizeError(err)
	}
	dir := &MountDir{
		virtualPath: virtualPath,
		hostPath:    hostPath,
		mode:        config.mode,
		handle:      handle,
	}
	// AddCleanup captures the handle value (not dir) so a dropped MountDir
	// that was never Closed still frees the Rust handle. Close stops it first,
	// so the handle is freed exactly once.
	dir.cleanup = runtime.AddCleanup(dir, ffi.MountFree, handle)
	return dir, nil
}

// Close releases the Rust-side mount handle. Close is idempotent.
func (m *MountDir) Close() {
	if m == nil {
		return
	}
	m.cleanup.Stop()
	m.mu.Lock()
	handle := m.handle
	m.handle = 0
	m.mu.Unlock()
	ffi.MountFree(handle)
}

// VirtualPath returns the POSIX path prefix visible to Python.
func (m *MountDir) VirtualPath() string {
	if m == nil {
		return ""
	}
	return m.virtualPath
}

// HostPath returns the host directory backing the mount.
func (m *MountDir) HostPath() string {
	if m == nil {
		return ""
	}
	return m.hostPath
}

// Mode returns the mount access policy.
func (m *MountDir) Mode() MountMode {
	if m == nil {
		return MountOverlay
	}
	return m.mode
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

func cleanVirtualPath(virtualPath string) string {
	if virtualPath == "" {
		virtualPath = "/"
	} else if virtualPath[0] != '/' {
		virtualPath = "/" + virtualPath
	}
	return path.Clean(virtualPath)
}

// --------------------------------------------------------------------------
// Run-config OS dispatch
// --------------------------------------------------------------------------

// openMounts resolves the run's MountDir handles up front so a closed mount
// fails the run before execution starts.
func (c *runConfig) openMounts() error {
	if len(c.mounts) == 0 {
		return nil
	}
	for _, mount := range c.mounts {
		if _, err := mount.ffiHandle(); err != nil {
			return err
		}
	}
	return nil
}

// mountFFIHandles snapshots the mount handles for one FFI call.
func (c *runConfig) mountFFIHandles() []uintptr {
	if len(c.mounts) == 0 {
		return nil
	}
	handles := make([]uintptr, 0, len(c.mounts))
	for _, mount := range c.mounts {
		if handle, err := mount.ffiHandle(); err == nil {
			handles = append(handles, handle)
		}
	}
	return handles
}

// hasOSDispatch reports whether any OS-call answering is configured.
func (c *runConfig) hasOSDispatch() bool {
	return len(c.mounts) != 0 || len(c.fsMounts) != 0 || c.osHandler != nil
}

// dispatchOS answers one surfaced OS call: mounts first, then WithFS
// filesystems, then the OS handler. ErrNotHandled means nothing claimed it.
func (c *runConfig) dispatchOS(ctx context.Context, call *Call) (Value, error) {
	fn := OSFunction(call.Name)
	if len(c.mounts) != 0 && fn.IsFilesystem() && len(call.Args) > 0 {
		value, handled, err := c.dispatchMounts(call)
		if err != nil {
			return Value{}, err
		}
		if handled {
			return value, nil
		}
	}
	return dispatchOSCall(ctx, c.fsMounts, c.osHandler, fn, call.Args, call.Kwargs)
}

// dispatchMounts runs one OS call against the Rust mount table.
func (c *runConfig) dispatchMounts(call *Call) (Value, bool, error) {
	arena := &rawArena{}
	rawArgs, err := valuesToRaw(call.Args, arena)
	if err != nil {
		return Value{}, true, err
	}
	defer freeOwnedRawValues(rawArgs)
	rawKwargs := make([]ffi.RawPair, 0, len(call.Kwargs))
	for name, value := range call.Kwargs {
		raw, err := valueToRaw(value, arena)
		if err != nil {
			freeOwnedRawPairs(rawKwargs)
			return Value{}, true, err
		}
		key, err := valueToRaw(Str(name), arena)
		if err != nil {
			freeOwnedRawValue(&raw)
			freeOwnedRawPairs(rawKwargs)
			return Value{}, true, err
		}
		rawKwargs = append(rawKwargs, ffi.RawPair{Key: key, Value: raw})
	}
	defer freeOwnedRawPairs(rawKwargs)
	result, handled, err := ffi.MountHandleOSCall(c.mountFFIHandles(), call.Name, rawArgs, rawKwargs)
	runtime.KeepAlive(arena)
	runtime.KeepAlive(call)
	if err != nil {
		return Value{}, handled, normalizeError(err)
	}
	if !handled {
		return Value{}, false, nil
	}
	value, err := decodeRawValue(result)
	return value, true, err
}

// dispatchOSCall answers one OS call from Go-side sources: WithFS read-only
// filesystems first, then the OS handler.
func dispatchOSCall(ctx context.Context, fsMounts []fsMount, handler OSHandler, fn OSFunction, args []Value, kwargs map[string]Value) (Value, error) {
	if fn.IsFilesystem() && len(args) > 0 {
		for i := range fsMounts {
			value, handled, err := fsMounts[i].handle(fn, args, kwargs)
			if err != nil {
				return Value{}, err
			}
			if handled {
				return value, nil
			}
		}
	}
	if handler != nil {
		result, err := handler(ctx, fn, args, kwargs)
		if err != nil {
			return Value{}, err
		}
		return From(result)
	}
	return Value{}, ErrNotHandled
}
