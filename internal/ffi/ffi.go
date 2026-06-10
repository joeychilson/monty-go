// Package ffi is the purego binding to the Rust monty-ffi cdylib.
//
// Layout-mirrored structs and constants here must stay in lockstep with the
// #[repr(C)] definitions in crates/monty-ffi; both sides assert layouts in
// tests and the loader verifies mg_abi_version at startup.
package ffi

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// AbiVersion is the C ABI revision this binding was built against. The loaded
// library must report the same value from mg_abi_version.
const AbiVersion = 3

const (
	// StatusOK is returned by every successful FFI call. Non-zero statuses mean
	// an error handle was written to the trailing out-pointer; pass it to
	// TakeError.
	StatusOK = 0
	// StatusErrRetained is returned by resume entry points whose payload was
	// rejected before the progress handle was consumed: the caller still owns
	// the live handle and the paused state is retryable.
	StatusErrRetained = 2
)

// RetainedError wraps a resume failure that did not consume the progress
// handle (StatusErrRetained): the paused state is still live, so the caller
// should restore its bookkeeping instead of treating the run as failed.
type RetainedError struct{ Err error }

func (e *RetainedError) Error() string { return e.Err.Error() }
func (e *RetainedError) Unwrap() error { return e.Err }

// Encoding of values returned from the "fast" run path.
const (
	FastFormatRaw  = 0
	FastFormatFlat = 1
)

// Progress kinds reported in ProgressSnapshot; describe why the Rust runtime
// suspended execution and what input it expects on resume.
const (
	ProgressFunctionCall   = 1
	ProgressOSCall         = 2
	ProgressResolveFutures = 3
	ProgressNameLookup     = 4
	ProgressComplete       = 5
)

// Outcomes a host can report back to a pending future via FutureResult.Kind.
const (
	FutureResultReturn   = 0
	FutureResultError    = 1
	FutureResultNotFound = 2
)

// Outcomes a host callback reports for one dispatched call.
const (
	HostCallbackReturn     = 0
	HostCallbackException  = 1
	HostCallbackNotHandled = 2
)

// Kinds of host callback requests.
const (
	HostCallFunction = 1
	HostCallOS       = 2
)

// Print payload encodings; see Printed.
const (
	PrintPlain  = 0
	PrintTagged = 1
)

// Print stream tags within a tagged payload.
const (
	StreamStdout = 0
	StreamStderr = 1
)

// RawValue.Kind discriminants. Values mirror the Rust mg_kind enum and must
// stay in lockstep with the C ABI definition.
const (
	KindInvalid         = 0
	KindEllipsis        = 1
	KindNone            = 2
	KindBool            = 3
	KindInt             = 4
	KindBigInt          = 5
	KindFloat           = 6
	KindString          = 7
	KindBytes           = 8
	KindList            = 9
	KindTuple           = 10
	KindNamedTuple      = 11
	KindDict            = 12
	KindSet             = 13
	KindFrozenSet       = 14
	KindDate            = 15
	KindDateTime        = 16
	KindTimeDelta       = 17
	KindTimeZone        = 18
	KindException       = 19
	KindType            = 20
	KindBuiltinFunction = 21
	KindPath            = 22
	KindDataclass       = 23
	KindFunction        = 24
	KindRepr            = 25
	KindCycle           = 26
	// KindOwnedHandle marks a RawValue whose payload is an owned uintptr
	// handle (RawValue.Handle); the holder is responsible for freeing it
	// with mg_value_free.
	KindOwnedHandle = ^uint32(0)
)

// Str is a borrowed (ptr, len) pair pointing at Go-owned string memory. The
// pointee must outlive the FFI call; never store Str across calls.
//
// Ptr is an unsafe.Pointer rather than a uintptr so the garbage collector
// traces it: any reachable Str (e.g. embedded in a struct passed by pointer,
// or held in a []Str passed via slicePointer) transitively keeps its backing
// string alive for the duration of the call. The C ABI is unchanged — Rust
// sees *const u8, usize either way.
type Str struct {
	Ptr unsafe.Pointer
	Len uintptr
}

// Bytes is a (ptr, len) pair returned by the Rust side. The buffer is owned
// by the FFI runtime and must be released with mgBytesFree (use TakeBytes,
// TakeString, or MaybeBytesFree to handle this safely).
type Bytes struct {
	Ptr unsafe.Pointer
	Len uintptr
}

// Limits mirrors the C struct that bounds resource consumption for a single
// program run. Each *Set field is a boolean toggle that activates the value
// in the field below it. CancelToken optionally attaches a cancellation flag
// (from CancelTokenNew) observed at Python statement boundaries.
type Limits struct {
	MaxAllocationsSet uint8
	_                 [7]byte
	MaxAllocations    uintptr

	MaxDurationNanosSet uint8
	_                   [7]byte
	MaxDurationNanos    uint64

	MaxMemorySet uint8
	_            [7]byte
	MaxMemory    uintptr

	GCIntervalSet uint8
	_             [7]byte
	GCInterval    uintptr

	MaxRecursionDepthSet uint8
	_                    [7]byte
	MaxRecursionDepth    uintptr

	DisableRecursionLimit uint8
	_                     [7]byte
	CancelToken           uintptr
}

// ProgramCompileArgs mirrors MgProgramCompileArgs in the Rust FFI crate.
type ProgramCompileArgs struct {
	Code       Str
	ScriptName Str
	InputNames unsafe.Pointer
	InputCount uintptr
}

// CompileRunFastRawArgs mirrors MgCompileRunFastRawArgs in the Rust FFI crate.
// It bundles compile + run + free into a single FFI hop.
type CompileRunFastRawArgs struct {
	Code            Str
	ScriptName      Str
	InputNames      unsafe.Pointer
	InputCount      uintptr
	InputValues     unsafe.Pointer
	InputValueCount uintptr
	Limits          *Limits
}

// RunHostArgs mirrors MgRunHostArgs: a single-hop host-dispatch run with
// registered function names, mounts, the unified host callback, and an
// optional streaming print callback.
type RunHostArgs struct {
	Inputs        unsafe.Pointer
	InputCount    uintptr
	Limits        *Limits
	HostNames     unsafe.Pointer
	HostNameCount uintptr
	Mounts        unsafe.Pointer
	MountCount    uintptr
	Callback      uintptr
	CallbackData  uintptr
	Print         uintptr
	PrintData     uintptr
}

// MountNewArgs mirrors MgMountNewArgs in the Rust FFI crate.
type MountNewArgs struct {
	VirtualPath        Str
	HostPath           Str
	Mode               uint32
	HasWriteBytesLimit uint8
	_                  [3]byte
	WriteBytesLimit    uint64
}

// MountCallArgs mirrors MgMountCallArgs in the Rust FFI crate.
type MountCallArgs struct {
	Mounts     unsafe.Pointer
	MountCount uintptr
	Function   Str
	Args       unsafe.Pointer
	ArgCount   uintptr
	Kwargs     unsafe.Pointer
	KwargCount uintptr
}

// ReplNewArgs mirrors MgReplNewArgs in the Rust FFI crate.
type ReplNewArgs struct {
	ScriptName Str
	Limits     unsafe.Pointer
}

// ReplFeedArgs mirrors MgReplFeedArgs: one REPL snippet execution with
// per-snippet controls and (for feed_run only) host dispatch.
type ReplFeedArgs struct {
	Code             Str
	InputNames       unsafe.Pointer
	InputValues      unsafe.Pointer
	InputCount       uintptr
	HasMaxDuration   uint8
	_                [7]byte
	MaxDurationNanos uint64
	CancelToken      uintptr
	HostNames        unsafe.Pointer
	HostNameCount    uintptr
	Mounts           unsafe.Pointer
	MountCount       uintptr
	Callback         uintptr
	CallbackData     uintptr
	Print            uintptr
	PrintData        uintptr
}

// ReplCallArgs mirrors MgReplCallArgs in the Rust FFI crate.
type ReplCallArgs struct {
	Name     Str
	Args     unsafe.Pointer
	ArgCount uintptr
}

// TypeCheckArgs mirrors MgTypeCheckArgs in the Rust FFI crate.
type TypeCheckArgs struct {
	Code       Str
	ScriptName Str
	Stubs      Str
	StubsName  Str
}

// FutureResult mirrors MgFutureResult and carries the host result for one
// pending async call.
type FutureResult struct {
	CallID  uint32
	Kind    uint32
	Value   RawValue
	ExcType Str
	Message Str
}

// RawValue is the C ABI representation of a Python value crossing the FFI
// boundary without an intermediate handle allocation. Only the fields
// matching Kind are valid; Ptr/Len fields point at Rust-owned buffers and
// must be freed with mgRawValueFree (use RawValueFree) when Kind designates
// a borrowed allocation. A Kind of KindOwnedHandle means Handle owns a
// uintptr that needs mg_value_free.
type RawValue struct {
	Kind   uint32
	Bool   uint8
	_      [3]byte
	Int    int64
	Float  float64
	Ptr    unsafe.Pointer
	Len    uintptr
	Handle uintptr
}

// RawPair is the C ABI representation of one key/value pair.
type RawPair struct {
	Key   RawValue
	Value RawValue
}

// RunJSONOutput is the bytes-returning output from JSON execution.
type RunJSONOutput struct {
	Value      Bytes
	Print      Bytes
	PrintFlags uint32
	_          uint32
	Error      uintptr
}

// FastScratchCap is the size of the inline scratch buffer the Rust side may
// use to return flat-format result bytes. Matches FAST_SCRATCH_CAP in the
// Rust crate; sized to cover every benchmark payload so callers avoid the
// extra mg_bytes_free cgocall on the common path.
const FastScratchCap = 8192

// RunFastOutput is the tagged output from the fastest available raw run path.
// Layout must match MgRunFastOutput in the Rust crate.
type RunFastOutput struct {
	Format uint32
	// BytesInScratch is 1 when Bytes.Ptr points inside Scratch (Go-owned, no
	// free required) and 0 when Bytes references a Rust-owned heap buffer
	// that must be released with mg_bytes_free.
	BytesInScratch uint32
	PrintFlags     uint32
	_              uint32
	Value          RawValue
	Bytes          Bytes
	Print          Bytes
	Error          uintptr
	// Scratch is filled by the Rust side when a flat-encoded result fits.
	// Keep it last; its size dominates the pooled-allocation cost.
	Scratch [FastScratchCap]byte
}

// ProgressSnapshot mirrors MgProgressSnapshot and stores a zero-copy view of a
// progress state.
type ProgressSnapshot struct {
	Kind       uint32
	CallID     uint32
	MethodCall uint8
	_          [7]byte
	Name       Bytes
	Args       RawValue
	Kwargs     RawValue
	Value      RawValue
}

// ProgressSnapshotOutput mirrors MgProgressSnapshotOutput: start and resume
// calls return the next progress handle and its snapshot in one hop. Progress
// is 0 when the run finished. Repl is non-zero when a REPL execution handed
// its session back (at completion, or alongside Error when a Python exception
// preserved the session).
type ProgressSnapshotOutput struct {
	Progress   uintptr
	Repl       uintptr
	Error      uintptr
	Print      Bytes
	PrintFlags uint32
	_          uint32
	Snapshot   ProgressSnapshot
}

// HostFunctionOutput is written by Go callbacks invoked from Rust.
type HostFunctionOutput struct {
	Value   RawValue
	ExcType Str
	Message Str
}

// MountOutput is returned by mounted OS call handling.
type MountOutput struct {
	Value   RawValue
	Error   uintptr
	Handled uint8
	_       [7]byte
}

// Date mirrors MgDate in the Rust FFI crate.
type Date struct {
	Year  int32
	Month uint8
	Day   uint8
	_     [2]byte
}

// DateTime mirrors MgDateTime in the Rust FFI crate.
type DateTime struct {
	TimezoneName  Bytes
	Year          int32
	Microsecond   uint32
	OffsetSeconds int32
	Month         uint8
	Day           uint8
	Hour          uint8
	Minute        uint8
	Second        uint8
	HasOffset     uint8
	HasTimezone   uint8
	_             uint8
}

// TimeDelta mirrors MgTimeDelta in the Rust FFI crate.
type TimeDelta struct {
	Days         int32
	Seconds      int32
	Microseconds int32
}

// TimeZone mirrors MgTimeZone in the Rust FFI crate.
type TimeZone struct {
	Name          Bytes
	OffsetSeconds int32
	HasName       uint8
	_             [3]byte
}

// DataclassRawArgs mirrors MgDataclassRawArgs in the Rust FFI crate.
type DataclassRawArgs struct {
	Name       Str
	TypeID     uint64
	FieldNames unsafe.Pointer
	FieldCount uintptr
	Attrs      unsafe.Pointer
	AttrCount  uintptr
	Frozen     uint8
	_          [7]byte
}

// Frame is one decoded traceback frame from an Error.
type Frame struct {
	File          string
	Function      string
	SourceLine    string
	Line          int
	Column        int
	EndLine       int
	EndColumn     int
	HasFunction   bool
	HasSourceLine bool
	HideCaret     bool
	HideFrameName bool
}

// Error is the Go form of a Rust-side panic or Python exception that escaped
// the FFI boundary. Type and Message are taken from Rust; Display is the
// pre-formatted string Rust would print (a full traceback rendering for
// Python runtime errors) and Traceback holds the structured frames,
// outermost first (empty for synthetic FFI errors).
type Error struct {
	Type      string
	Message   string
	Display   string
	Traceback []Frame
}

// Error formats the FFI error as a Go error string.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return e.Type
	}
	return e.Type + ": " + e.Message
}

// Printed is the print output collected during one FFI hop. Flags selects the
// encoding of Data: PrintPlain means Data is stdout text verbatim (the common
// case); PrintTagged means Data is a sequence of [stream u8][len u32 le]
// [bytes] chunks preserving stdout/stderr interleaving.
type Printed struct {
	Flags uint32
	Data  string
}

// Empty reports whether no output was produced.
func (p Printed) Empty() bool { return p.Data == "" }

// ForEach invokes fn for every output chunk in emit order. Malformed tagged
// payloads stop with an error.
func (p Printed) ForEach(fn func(stream uint8, text string) error) error {
	if p.Data == "" {
		return nil
	}
	if p.Flags == PrintPlain {
		return fn(StreamStdout, p.Data)
	}
	data := p.Data
	for data != "" {
		if len(data) < 5 {
			return errors.New("monty: truncated tagged print payload")
		}
		stream := data[0]
		size := binary.LittleEndian.Uint32([]byte(data[1:5]))
		if len(data)-5 < int(size) {
			return errors.New("monty: truncated tagged print chunk")
		}
		if err := fn(stream, data[5:5+size]); err != nil {
			return err
		}
		data = data[5+size:]
	}
	return nil
}

// TakePrinted consumes a Rust-owned print buffer into a Printed value.
func TakePrinted(buf Bytes, flags uint32) Printed {
	return Printed{Flags: flags, Data: TakeString(buf)}
}

// StepResult is the uniform outcome of starting or resuming a suspendable
// execution. Progress is 0 when the run finished; Repl is non-zero when a
// REPL session was handed back (ownership transfers to the caller, including
// on the error path).
type StepResult struct {
	Progress uintptr
	Repl     uintptr
	Snapshot ProgressSnapshot
	Print    Printed
}

var (
	loadOnce sync.Once
	loadErr  error
	lib      uintptr

	// Buffer, error, and cancellation symbols.
	mgBytesFree func(unsafe.Pointer, uintptr)

	mgRawValueFree func(unsafe.Pointer)

	mgErrorFree    func(uintptr)
	mgErrorDetails func(uintptr, unsafe.Pointer, unsafe.Pointer) int32

	mgAbiVersion func() uint32

	mgCancelTokenNew    func() uintptr
	mgCancelTokenCancel func(uintptr)
	mgCancelTokenFree   func(uintptr)

	// Type checking symbols.
	mgTypeCheck         func(unsafe.Pointer, unsafe.Pointer, unsafe.Pointer) int32
	mgDiagnosticsRender func(uintptr, unsafe.Pointer, uintptr, uint8, unsafe.Pointer, unsafe.Pointer) int32
	mgDiagnosticsFree   func(uintptr)

	// Mount and REPL symbols.
	mgMountNew             func(unsafe.Pointer, unsafe.Pointer, unsafe.Pointer) int32
	mgMountFree            func(uintptr)
	mgMountHandleOSCall    func(unsafe.Pointer, unsafe.Pointer) int32
	mgReplNew              func(unsafe.Pointer, unsafe.Pointer, unsafe.Pointer) int32
	mgReplFree             func(uintptr)
	mgReplDump             func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgReplLoad             func(unsafe.Pointer, uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgReplFunctionNames    func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgReplHasFunction      func(uintptr, unsafe.Pointer, uintptr) uint8
	mgReplContinuationMode func(unsafe.Pointer, uintptr) uint32
	mgReplFeedRunRawAddr   uintptr
	mgReplFeedStartAddr    uintptr
	mgReplCallRawAddr      uintptr

	// Program symbols.
	mgProgramCompile              func(unsafe.Pointer, unsafe.Pointer, unsafe.Pointer) int32
	mgProgramFree                 func(uintptr)
	mgProgramDump                 func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgProgramCode                 func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgProgramScriptName           func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgProgramInputNames           func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgProgramLoad                 func(unsafe.Pointer, uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgProgramStartRawSnapshotAddr uintptr
	mgProgramRunHostRawAddr       uintptr
	mgProgramRunFastAddr          uintptr
	mgProgramCompileRunFastAddr   uintptr
	mgProgramRunJSONAddr          uintptr

	// Value symbols.
	mgValueFree                 func(uintptr)
	mgValueJSON                 func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgValueEllipsis             func() uintptr
	mgValueNone                 func() uintptr
	mgValueBool                 func(uint8) uintptr
	mgValueInt                  func(int64) uintptr
	mgValueBigInt               func(unsafe.Pointer, uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgValueFloat                func(float64) uintptr
	mgValueString               func(unsafe.Pointer, uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgValuePath                 func(unsafe.Pointer, uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgValueBytes                func(unsafe.Pointer, uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgValueListRaw              func(unsafe.Pointer, uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgValueTupleRaw             func(unsafe.Pointer, uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgValueNamedTupleRaw        func(unsafe.Pointer, uintptr, unsafe.Pointer, unsafe.Pointer, uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgValueSetRaw               func(unsafe.Pointer, uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgValueFrozenSetRaw         func(unsafe.Pointer, uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgValueDictRaw              func(unsafe.Pointer, uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgValueDate                 func(unsafe.Pointer, unsafe.Pointer, unsafe.Pointer) int32
	mgValueDateTime             func(unsafe.Pointer, unsafe.Pointer, unsafe.Pointer) int32
	mgValueTimeDelta            func(unsafe.Pointer, unsafe.Pointer, unsafe.Pointer) int32
	mgValueTimeZone             func(unsafe.Pointer, unsafe.Pointer, unsafe.Pointer) int32
	mgValueDataclassRaw         func(unsafe.Pointer, unsafe.Pointer, unsafe.Pointer) int32
	mgValueFunction             func(unsafe.Pointer, uintptr, unsafe.Pointer, uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgValueException            func(unsafe.Pointer, uintptr, unsafe.Pointer, uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgValueExceptionType        func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgValueExceptionMessage     func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgValueKind                 func(uintptr) uint32
	mgValueBoolGet              func(uintptr) uint8
	mgValueIntGet               func(uintptr) int64
	mgValueFloatGet             func(uintptr) float64
	mgValueText                 func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgValueBytesGet             func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgValueDateGet              func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgValueDateTimeGet          func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgValueTimeDeltaGet         func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgValueTimeZoneGet          func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgValueNamedTupleTypeName   func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgValueNamedTupleFieldNames func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgValueDataclassName        func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgValueDataclassTypeID      func(uintptr) uint64
	mgValueDataclassFieldNames  func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgValueDataclassFrozen      func(uintptr) uint8
	mgValueLen                  func(uintptr) uintptr
	mgValueItemsRaw             func(uintptr, unsafe.Pointer, uintptr) int32
	mgValuePairsRaw             func(uintptr, unsafe.Pointer, uintptr) int32

	// Progress symbols.
	mgProgressFree                    func(uintptr)
	mgProgressSnapshot                func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgProgressPendingLen              func(uintptr) uintptr
	mgProgressPendingID               func(uintptr, uintptr) uint32
	mgProgressIsRepl                  func(uintptr) uint8
	mgProgressSetCancelToken          func(uintptr, uintptr) uint8
	mgProgressCallJSON                func(uintptr, unsafe.Pointer, unsafe.Pointer, unsafe.Pointer) int32
	mgProgressDump                    func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgProgressLoad                    func(unsafe.Pointer, uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgProgressResumeReturnRawAddr     uintptr
	mgProgressResumeExceptionAddr     uintptr
	mgProgressResumePendingAddr       uintptr
	mgProgressResumeNotHandledAddr    uintptr
	mgProgressResumeNameValueRawAddr  uintptr
	mgProgressResumeNameUndefinedAddr uintptr
	mgProgressResumeFuturesAddr       uintptr
)

// EnsureLoaded resolves the Rust shared library on first call and registers
// every FFI symbol. Safe to call from multiple goroutines: the work happens
// inside a sync.Once, subsequent callers observe the cached result.
func EnsureLoaded() error {
	loadOnce.Do(func() {
		if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
			loadErr = fmt.Errorf("monty: unsupported OS %s", runtime.GOOS)
			return
		}
		path, err := findLibrary()
		if err != nil {
			loadErr = err
			return
		}
		lib, err = purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			loadErr = fmt.Errorf("monty: load %s: %w", path, err)
			return
		}
		loadErr = registerSymbols(path)
	})
	return loadErr
}

func findLibrary() (string, error) {
	if path := os.Getenv("MONTY_GO_LIB"); path != "" {
		if !filepath.IsAbs(path) {
			return "", fmt.Errorf("monty: MONTY_GO_LIB must be an absolute path, got %q", path)
		}
		//nolint:gosec // G703: MONTY_GO_LIB is an operator-provided library path; validating its existence is the intent
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("monty: MONTY_GO_LIB %q is not accessible: %w", path, err)
		}
		return path, nil
	}
	name := libraryFileName()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("monty: cannot locate internal ffi package")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	candidates := []string{
		filepath.Join(root, "crates", "monty-ffi", "target", "release", name),
		filepath.Join(root, "target", "release", name),
		filepath.Join(root, name),
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	embeddedPath, err := embeddedLibraryPath(name)
	if err == nil {
		return embeddedPath, nil
	}
	if !errors.Is(err, errEmbeddedLibraryUnavailable) {
		return "", err
	}
	return "", fmt.Errorf("monty: Rust shared library %s not found; run `cargo build --release -p monty-ffi`, set MONTY_GO_LIB, or build with embedded FFI assets", name)
}

func libraryFileName() string {
	switch runtime.GOOS {
	case "darwin":
		return "libmonty_ffi.dylib"
	case "linux":
		return "libmonty_ffi.so"
	default:
		return "libmonty_ffi"
	}
}

// registerSymbols resolves and binds every FFI symbol from the loaded library.
// It returns a descriptive error (rather than panicking) when a symbol is
// missing or the ABI revision mismatches — the common case when an old
// library is found via MONTY_GO_LIB or a stale build directory. Errors must
// be sticky: registerSymbols runs inside loadOnce, which is never retried, so
// a failure here is recorded in loadErr and reported to every subsequent
// caller. The deferred recover is a backstop for purego.RegisterFunc, which
// panics on an unsupported function signature (a programmer error);
// converting it to loadErr keeps the Once from completing with
// half-registered, nil function pointers that would crash on first use.
func registerSymbols(path string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("monty: register FFI symbols from %s: %v", path, r)
		}
	}()

	// The ABI handshake comes first: layouts may have drifted even when every
	// symbol resolves.
	versionAddr, derr := purego.Dlsym(lib, "mg_abi_version")
	if derr != nil {
		return fmt.Errorf("monty: symbol mg_abi_version not found in %s; library is older than the Go binding: %w", path, derr)
	}
	purego.RegisterFunc(&mgAbiVersion, versionAddr)
	if version := mgAbiVersion(); version != AbiVersion {
		return fmt.Errorf("monty: library %s reports ABI version %d, Go binding requires %d; rebuild monty-ffi", path, version, AbiVersion)
	}

	symbols := []struct {
		name   string
		target any
	}{
		{"mg_bytes_free", &mgBytesFree},
		{"mg_raw_value_free", &mgRawValueFree},
		{"mg_error_free", &mgErrorFree},
		{"mg_error_details", &mgErrorDetails},
		{"mg_cancel_token_new", &mgCancelTokenNew},
		{"mg_cancel_token_cancel", &mgCancelTokenCancel},
		{"mg_cancel_token_free", &mgCancelTokenFree},
		{"mg_type_check", &mgTypeCheck},
		{"mg_diagnostics_render", &mgDiagnosticsRender},
		{"mg_diagnostics_free", &mgDiagnosticsFree},
		{"mg_mount_new", &mgMountNew},
		{"mg_mount_free", &mgMountFree},
		{"mg_mount_handle_os_call", &mgMountHandleOSCall},
		{"mg_repl_new", &mgReplNew},
		{"mg_repl_free", &mgReplFree},
		{"mg_repl_dump", &mgReplDump},
		{"mg_repl_load", &mgReplLoad},
		{"mg_repl_function_names", &mgReplFunctionNames},
		{"mg_repl_has_function", &mgReplHasFunction},
		{"mg_repl_continuation_mode", &mgReplContinuationMode},
		{"mg_program_compile", &mgProgramCompile},
		{"mg_program_free", &mgProgramFree},
		{"mg_program_dump", &mgProgramDump},
		{"mg_program_code", &mgProgramCode},
		{"mg_program_script_name", &mgProgramScriptName},
		{"mg_program_input_names", &mgProgramInputNames},
		{"mg_program_load", &mgProgramLoad},
		{"mg_value_free", &mgValueFree},
		{"mg_value_json", &mgValueJSON},
		{"mg_value_ellipsis", &mgValueEllipsis},
		{"mg_value_none", &mgValueNone},
		{"mg_value_bool", &mgValueBool},
		{"mg_value_int", &mgValueInt},
		{"mg_value_big_int", &mgValueBigInt},
		{"mg_value_float", &mgValueFloat},
		{"mg_value_string", &mgValueString},
		{"mg_value_path", &mgValuePath},
		{"mg_value_bytes", &mgValueBytes},
		{"mg_value_list_raw", &mgValueListRaw},
		{"mg_value_tuple_raw", &mgValueTupleRaw},
		{"mg_value_named_tuple_raw", &mgValueNamedTupleRaw},
		{"mg_value_set_raw", &mgValueSetRaw},
		{"mg_value_frozen_set_raw", &mgValueFrozenSetRaw},
		{"mg_value_dict_raw", &mgValueDictRaw},
		{"mg_value_date", &mgValueDate},
		{"mg_value_datetime", &mgValueDateTime},
		{"mg_value_timedelta", &mgValueTimeDelta},
		{"mg_value_timezone", &mgValueTimeZone},
		{"mg_value_dataclass_raw", &mgValueDataclassRaw},
		{"mg_value_function", &mgValueFunction},
		{"mg_value_exception", &mgValueException},
		{"mg_value_exception_type", &mgValueExceptionType},
		{"mg_value_exception_message", &mgValueExceptionMessage},
		{"mg_value_kind", &mgValueKind},
		{"mg_value_bool_get", &mgValueBoolGet},
		{"mg_value_int_get", &mgValueIntGet},
		{"mg_value_float_get", &mgValueFloatGet},
		{"mg_value_text", &mgValueText},
		{"mg_value_bytes_get", &mgValueBytesGet},
		{"mg_value_date_get", &mgValueDateGet},
		{"mg_value_datetime_get", &mgValueDateTimeGet},
		{"mg_value_timedelta_get", &mgValueTimeDeltaGet},
		{"mg_value_timezone_get", &mgValueTimeZoneGet},
		{"mg_value_named_tuple_type_name", &mgValueNamedTupleTypeName},
		{"mg_value_named_tuple_field_names", &mgValueNamedTupleFieldNames},
		{"mg_value_dataclass_name", &mgValueDataclassName},
		{"mg_value_dataclass_type_id", &mgValueDataclassTypeID},
		{"mg_value_dataclass_field_names", &mgValueDataclassFieldNames},
		{"mg_value_dataclass_frozen", &mgValueDataclassFrozen},
		{"mg_value_len", &mgValueLen},
		{"mg_value_items_raw", &mgValueItemsRaw},
		{"mg_value_pairs_raw", &mgValuePairsRaw},
		{"mg_progress_free", &mgProgressFree},
		{"mg_progress_snapshot", &mgProgressSnapshot},
		{"mg_progress_pending_len", &mgProgressPendingLen},
		{"mg_progress_pending_id", &mgProgressPendingID},
		{"mg_progress_is_repl", &mgProgressIsRepl},
		{"mg_progress_set_cancel_token", &mgProgressSetCancelToken},
		{"mg_progress_call_json", &mgProgressCallJSON},
		{"mg_progress_dump", &mgProgressDump},
		{"mg_progress_load", &mgProgressLoad},
	}
	for _, symbol := range symbols {
		addr, derr := purego.Dlsym(lib, symbol.name)
		if derr != nil {
			return fmt.Errorf("monty: symbol %s not found in %s; library is older than the Go binding: %w", symbol.name, path, derr)
		}
		// Equivalent to purego.RegisterLibFunc, but resolving the address
		// ourselves lets a missing symbol return an error instead of panicking.
		purego.RegisterFunc(symbol.target, addr)
	}

	// Symbols invoked through hand-rolled syscall trampolines, bound by raw
	// address rather than a Go func pointer.
	rawSymbols := []struct {
		name   string
		target *uintptr
	}{
		{"mg_program_start_raw_snapshot", &mgProgramStartRawSnapshotAddr},
		{"mg_program_run_host_raw", &mgProgramRunHostRawAddr},
		{"mg_program_run_fast_raw", &mgProgramRunFastAddr},
		{"mg_program_compile_run_fast_raw", &mgProgramCompileRunFastAddr},
		{"mg_program_run_json_raw", &mgProgramRunJSONAddr},
		{"mg_repl_feed_run_raw", &mgReplFeedRunRawAddr},
		{"mg_repl_feed_start", &mgReplFeedStartAddr},
		{"mg_repl_call_raw", &mgReplCallRawAddr},
		{"mg_progress_resume_return_raw", &mgProgressResumeReturnRawAddr},
		{"mg_progress_resume_exception", &mgProgressResumeExceptionAddr},
		{"mg_progress_resume_pending", &mgProgressResumePendingAddr},
		{"mg_progress_resume_not_handled", &mgProgressResumeNotHandledAddr},
		{"mg_progress_resume_name_value_raw", &mgProgressResumeNameValueRawAddr},
		{"mg_progress_resume_name_undefined", &mgProgressResumeNameUndefinedAddr},
		{"mg_progress_resume_futures", &mgProgressResumeFuturesAddr},
	}
	for _, symbol := range rawSymbols {
		addr, derr := purego.Dlsym(lib, symbol.name)
		if derr != nil {
			return fmt.Errorf("monty: symbol %s not found in %s; library is older than the Go binding: %w", symbol.name, path, derr)
		}
		*symbol.target = addr
	}
	return nil
}

// StringRef returns a borrowed view of s as an FFI Str. The caller must
// ensure s outlives the FFI call; storing the result is unsafe.
func StringRef(s string) Str {
	if s == "" {
		return Str{}
	}
	return Str{Ptr: unsafe.Pointer(unsafe.StringData(s)), Len: uintptr(len(s))}
}

// stringArgs decomposes s into the (ptr, len) pair a C function expects. The
// pointer is an unsafe.Pointer so purego keeps the backing string alive across
// the call; callers must still pass it to a function whose matching parameter
// is typed unsafe.Pointer (not uintptr) for that protection to apply.
func stringArgs(s string) (unsafe.Pointer, uintptr) {
	ref := StringRef(s)
	return ref.Ptr, ref.Len
}

// StringRefs converts a string slice into borrowed Str views. The original
// strings must outlive the FFI call; keep the returned slice alive too.
func StringRefs(values []string) []Str {
	if len(values) == 0 {
		return nil
	}
	refs := make([]Str, len(values))
	for i, value := range values {
		refs[i] = StringRef(value)
	}
	return refs
}

func slicePointer[T any](values []T) unsafe.Pointer {
	if len(values) == 0 {
		return nil
	}
	return unsafe.Pointer(unsafe.SliceData(values))
}

func sliceAddress[T any](values []T) uintptr {
	return uintptr(slicePointer(values))
}

// BytesRef returns a borrowed view of b suitable for passing to an FFI call
// that does not retain it. The slice must outlive the call.
func BytesRef(b []byte) (unsafe.Pointer, uintptr) {
	return slicePointer(b), uintptr(len(b))
}

// TakeBytes copies a Rust-owned buffer into a Go-owned []byte and frees the
// source. Use this whenever the buffer's data must outlive the FFI call.
func TakeBytes(buf Bytes) []byte {
	if buf.Ptr == nil {
		return nil
	}
	defer mgBytesFree(buf.Ptr, buf.Len)
	return append([]byte(nil), unsafe.Slice((*byte)(buf.Ptr), buf.Len)...)
}

// MaybeBytesFree releases a Rust-owned buffer if one is present. Use after
// an error path where the buffer was filled but its contents are unwanted.
func MaybeBytesFree(buf Bytes) {
	if buf.Ptr != nil {
		mgBytesFree(buf.Ptr, buf.Len)
	}
}

// UnsafeBytes exposes a Rust-owned buffer as a zero-copy []byte. The caller
// must not retain the slice past the next FFI call and is responsible for
// freeing the underlying buffer via mgBytesFree / MaybeBytesFree.
func UnsafeBytes(buf Bytes) []byte {
	if buf.Ptr == nil {
		return nil
	}
	return unsafe.Slice((*byte)(buf.Ptr), buf.Len)
}

// TakeString consumes a Rust-owned buffer and returns its contents as a Go
// string, freeing the source. Converting a []byte to string is a single
// alloc+copy, so there is nothing to optimize over string(unsafe.Slice(...)).
func TakeString(buf Bytes) string {
	if buf.Ptr == nil {
		return ""
	}
	defer mgBytesFree(buf.Ptr, buf.Len)
	return string(unsafe.Slice((*byte)(buf.Ptr), buf.Len))
}

// RawValueFree releases any Rust-owned payload referenced by a RawValue.
// Safe to call on a zero value or nil pointer.
func RawValueFree(value *RawValue) {
	if value != nil {
		mgRawValueFree(ptrOf(value))
	}
}

// Traceback frame flag bits in the mg_error_details encoding.
const (
	frameHasFunction   = 1
	frameHasSource     = 1 << 1
	frameHideCaret     = 1 << 2
	frameHideFrameName = 1 << 3
)

// TakeError consumes a Rust-owned error handle and converts it to a Go
// *Error (including the traceback frames), freeing the handle. Returns nil
// when handle == 0.
func TakeError(handle uintptr) error {
	if handle == 0 {
		return nil
	}
	defer mgErrorFree(handle)
	var details Bytes
	var errHandle uintptr
	if status := mgErrorDetails(handle, ptrOf(&details), ptrOf(&errHandle)); status != StatusOK {
		// Details encoding failed (out of memory); free the nested handle and
		// fall back to an opaque error.
		mgErrorFree(errHandle)
		return &Error{Type: "RuntimeError", Message: "failed to decode FFI error details"}
	}
	decoded, err := decodeErrorDetails(TakeString(details))
	if err != nil {
		return &Error{Type: "RuntimeError", Message: err.Error()}
	}
	return decoded
}

// decodeErrorDetails parses the flat buffer written by mg_error_details.
func decodeErrorDetails(data string) (*Error, error) {
	r := flatStringReader{data: data}
	out := &Error{}
	var err error
	if out.Type, err = r.str(); err != nil {
		return nil, err
	}
	if out.Message, err = r.str(); err != nil {
		return nil, err
	}
	if out.Display, err = r.str(); err != nil {
		return nil, err
	}
	count, err := r.u32()
	if err != nil {
		return nil, err
	}
	if count > 0 {
		out.Traceback = make([]Frame, 0, count)
	}
	for range count {
		var frame Frame
		line, err := r.u32()
		if err != nil {
			return nil, err
		}
		column, err := r.u32()
		if err != nil {
			return nil, err
		}
		endLine, err := r.u32()
		if err != nil {
			return nil, err
		}
		endColumn, err := r.u32()
		if err != nil {
			return nil, err
		}
		flags, err := r.u8()
		if err != nil {
			return nil, err
		}
		frame.Line, frame.Column = int(line), int(column)
		frame.EndLine, frame.EndColumn = int(endLine), int(endColumn)
		frame.HasFunction = flags&frameHasFunction != 0
		frame.HasSourceLine = flags&frameHasSource != 0
		frame.HideCaret = flags&frameHideCaret != 0
		frame.HideFrameName = flags&frameHideFrameName != 0
		if frame.File, err = r.str(); err != nil {
			return nil, err
		}
		if frame.HasFunction {
			if frame.Function, err = r.str(); err != nil {
				return nil, err
			}
		}
		if frame.HasSourceLine {
			if frame.SourceLine, err = r.str(); err != nil {
				return nil, err
			}
		}
		out.Traceback = append(out.Traceback, frame)
	}
	if !r.done() {
		return nil, errors.New("monty: trailing bytes in error details")
	}
	return out, nil
}

// flatStringReader decodes the little-endian length-prefixed encodings shared
// by the error-details payload.
type flatStringReader struct {
	data string
	pos  int
}

func (r *flatStringReader) u8() (uint8, error) {
	if r.pos+1 > len(r.data) {
		return 0, errors.New("monty: truncated error details")
	}
	v := r.data[r.pos]
	r.pos++
	return v, nil
}

func (r *flatStringReader) u32() (uint32, error) {
	if r.pos+4 > len(r.data) {
		return 0, errors.New("monty: truncated error details")
	}
	v := binary.LittleEndian.Uint32([]byte(r.data[r.pos : r.pos+4]))
	r.pos += 4
	return v, nil
}

func (r *flatStringReader) str() (string, error) {
	size, err := r.u32()
	if err != nil {
		return "", err
	}
	if r.pos+int(size) > len(r.data) {
		return "", errors.New("monty: truncated error details string")
	}
	v := r.data[r.pos : r.pos+int(size)]
	r.pos += int(size)
	return v, nil
}

func (r *flatStringReader) done() bool { return r.pos == len(r.data) }

func boolByte(v bool) uint8 {
	if v {
		return 1
	}
	return 0
}

func ptrOf[T any](v *T) unsafe.Pointer {
	if v == nil {
		return nil
	}
	return unsafe.Pointer(v)
}

// NewCallback registers a Go function as a C-callable pointer. The returned
// uintptr is process-lifetime; purego does not currently support releasing
// it, so callers should reuse callbacks rather than allocate per-call.
func NewCallback(fn any) uintptr {
	return purego.NewCallback(fn)
}
