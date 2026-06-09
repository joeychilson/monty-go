package ffi

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

const (
	// StatusOK is returned by every successful FFI call. Non-zero statuses mean
	// an error handle was written to the trailing out-pointer; pass it to
	// TakeError.
	StatusOK = 0
)

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

// Outcomes a host-function callback writes into HostFunctionOutput.
const (
	HostCallbackReturn    = 0
	HostCallbackException = 1
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
// in the field below it; the explicit padding keeps the layout identical to
// the Rust definition.
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
	Mounts      unsafe.Pointer
	MountCount  uintptr
	Function    Str
	Args        unsafe.Pointer
	ArgCount    uintptr
	KwargKeys   unsafe.Pointer
	KwargValues unsafe.Pointer
	KwargCount  uintptr
}

// ReplNewArgs mirrors MgReplNewArgs in the Rust FFI crate.
type ReplNewArgs struct {
	ScriptName Str
	Limits     unsafe.Pointer
}

// ReplFeedRunArgs mirrors MgReplFeedRunArgs in the Rust FFI crate.
type ReplFeedRunArgs struct {
	Code        Str
	InputNames  unsafe.Pointer
	InputValues unsafe.Pointer
	InputCount  uintptr
}

// ReplCallArgs mirrors MgReplCallArgs in the Rust FFI crate.
type ReplCallArgs struct {
	Name     Str
	Args     unsafe.Pointer
	ArgCount uintptr
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

// RunOutput is the handle-returning output from program and REPL execution.
type RunOutput struct {
	Value uintptr
	Print Bytes
	Error uintptr
}

// StartOutput is the handle-returning output from starting a program.
type StartOutput struct {
	Progress uintptr
	Print    Bytes
	Error    uintptr
}

// RunRawOutput is the RawValue-returning output from raw execution.
type RunRawOutput struct {
	Value RawValue
	Print Bytes
	Error uintptr
}

// RunJSONOutput is the bytes-returning output from JSON execution.
type RunJSONOutput struct {
	Value Bytes
	Print Bytes
	Error uintptr
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
	Value          RawValue
	Bytes          Bytes
	Print          Bytes
	Error          uintptr
	// Scratch is filled by the Rust side when a flat-encoded result fits.
	// Keep it last; its size dominates the pooled-allocation cost.
	Scratch [FastScratchCap]byte
}

// ProgressOutput is returned by resume operations that yield another progress
// handle.
type ProgressOutput struct {
	Progress uintptr
	Print    Bytes
	Error    uintptr
}

// ProgressSnapshot mirrors MgProgressSnapshot and stores a zero-copy view of a
// progress state.
type ProgressSnapshot struct {
	Kind       uint32
	Name       Bytes
	Args       RawValue
	Kwargs     RawValue
	Value      RawValue
	CallID     uint32
	MethodCall uint8
	_          [3]byte
	Error      uintptr
}

// HostFunctionOutput is written by Go callbacks invoked from Rust.
type HostFunctionOutput struct {
	Value   RawValue
	ExcType Str
	Message Str
}

// MountOutput is returned by mounted OS call handling.
type MountOutput struct {
	Value   uintptr
	Error   uintptr
	Handled uint8
}

// RawPair is the C ABI representation of one key/value pair.
type RawPair struct {
	Key   RawValue
	Value RawValue
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

// Error is the Go form of a Rust-side panic or Python exception that escaped
// the FFI boundary. Type and Message are taken from Rust; Display is the
// pre-formatted string Rust would print, and takes precedence when set.
type Error struct {
	Type    string
	Message string
	Display string
}

// Error formats the FFI error as a Go error string.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Display != "" {
		return e.Display
	}
	if e.Message == "" {
		return e.Type
	}
	return e.Type + ": " + e.Message
}

var (
	loadOnce sync.Once
	loadErr  error
	lib      uintptr

	// Buffer and error symbols.
	mgBytesFree func(unsafe.Pointer, uintptr)

	mgRawValueFree func(unsafe.Pointer)

	mgErrorFree    func(uintptr)
	mgErrorType    func(uintptr, unsafe.Pointer) int32
	mgErrorMessage func(uintptr, unsafe.Pointer) int32
	mgErrorDisplay func(uintptr, unsafe.Pointer) int32

	// Program, REPL, and mount symbols.
	// The final out-error argument is the 9th, which overflows the 8 arm64
	// integer registers onto the stack. purego's stack-placement path only
	// handles uintptr/Ptr (not unsafe.Pointer), so this parameter must stay
	// uintptr; it points at a local uintptr handle that carries no Go heap data
	// needing GC tracing, so 1.1's keep-alive requirement does not apply to it.
	// The string-pointer args stay unsafe.Pointer (they sit in registers).
	mgTypeCheck                 func(unsafe.Pointer, uintptr, unsafe.Pointer, uintptr, unsafe.Pointer, uintptr, unsafe.Pointer, uintptr, uintptr) int32
	mgMountNew                  func(unsafe.Pointer, unsafe.Pointer, unsafe.Pointer) int32
	mgMountFree                 func(uintptr)
	mgMountHandleOSCall         func(unsafe.Pointer, unsafe.Pointer) int32
	mgReplNew                   func(unsafe.Pointer, unsafe.Pointer, unsafe.Pointer) int32
	mgReplFree                  func(uintptr)
	mgReplDump                  func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgReplLoad                  func(unsafe.Pointer, uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgReplFeedRun               func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgReplCallFunction          func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgReplFunctionNames         func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgReplHasFunction           func(uintptr, unsafe.Pointer, uintptr) uint8
	mgReplContinuationMode      func(unsafe.Pointer, uintptr) uint32
	mgProgramCompile            func(unsafe.Pointer, unsafe.Pointer, unsafe.Pointer) int32
	mgProgramFree               func(uintptr)
	mgProgramDump               func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgProgramCode               func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgProgramScriptName         func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgProgramInputNames         func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgProgramLoad               func(unsafe.Pointer, uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgProgramStartRawAddr       uintptr
	mgProgramRunRawAddr         uintptr
	mgProgramRunHostRawAddr     uintptr
	mgProgramRunFastAddr        uintptr
	mgProgramCompileRunFastAddr uintptr
	mgProgramRunJSONAddr        uintptr

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
	mgProgressFree                func(uintptr)
	mgProgressSnapshotAddr        uintptr
	mgProgressPendingLen          func(uintptr) uintptr
	mgProgressPendingID           func(uintptr, uintptr) uint32
	mgProgressResumePending       func(uintptr, unsafe.Pointer) int32
	mgProgressResumeReturnRawAddr uintptr
	mgProgressResumeException     func(uintptr, unsafe.Pointer, uintptr, unsafe.Pointer, uintptr, unsafe.Pointer) int32
	mgProgressResumeNameValueRaw  func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgProgressResumeNameUndefined func(uintptr, unsafe.Pointer) int32
	mgProgressResumeFutures       func(uintptr, unsafe.Pointer, uintptr, unsafe.Pointer) int32
	mgProgressDump                func(uintptr, unsafe.Pointer, unsafe.Pointer) int32
	mgProgressLoad                func(unsafe.Pointer, uintptr, unsafe.Pointer, unsafe.Pointer) int32
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
// missing — the common case when an old library is found via MONTY_GO_LIB or a
// stale build directory. Errors must be sticky: registerSymbols runs inside
// loadOnce, which is never retried, so a failure here is recorded in loadErr
// and reported to every subsequent caller. The deferred recover is a backstop
// for purego.RegisterFunc, which panics on an unsupported function signature
// (a programmer error); converting it to loadErr keeps the Once from completing
// with half-registered, nil function pointers that would crash on first use.
func registerSymbols(path string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("monty: register FFI symbols from %s: %v", path, r)
		}
	}()

	symbols := []struct {
		name   string
		target any
	}{
		{"mg_bytes_free", &mgBytesFree},
		{"mg_raw_value_free", &mgRawValueFree},
		{"mg_error_free", &mgErrorFree},
		{"mg_error_type", &mgErrorType},
		{"mg_error_message", &mgErrorMessage},
		{"mg_error_display", &mgErrorDisplay},
		{"mg_type_check", &mgTypeCheck},
		{"mg_mount_new", &mgMountNew},
		{"mg_mount_free", &mgMountFree},
		{"mg_mount_handle_os_call", &mgMountHandleOSCall},
		{"mg_repl_new", &mgReplNew},
		{"mg_repl_free", &mgReplFree},
		{"mg_repl_dump", &mgReplDump},
		{"mg_repl_load", &mgReplLoad},
		{"mg_repl_feed_run", &mgReplFeedRun},
		{"mg_repl_call_function", &mgReplCallFunction},
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
		{"mg_progress_pending_len", &mgProgressPendingLen},
		{"mg_progress_pending_id", &mgProgressPendingID},
		{"mg_progress_resume_pending", &mgProgressResumePending},
		{"mg_progress_resume_exception", &mgProgressResumeException},
		{"mg_progress_resume_name_value_raw", &mgProgressResumeNameValueRaw},
		{"mg_progress_resume_name_undefined", &mgProgressResumeNameUndefined},
		{"mg_progress_resume_futures", &mgProgressResumeFutures},
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
		{"mg_program_start_raw", &mgProgramStartRawAddr},
		{"mg_program_run_raw", &mgProgramRunRawAddr},
		{"mg_program_run_host_raw", &mgProgramRunHostRawAddr},
		{"mg_program_run_fast_raw", &mgProgramRunFastAddr},
		{"mg_program_compile_run_fast_raw", &mgProgramCompileRunFastAddr},
		{"mg_program_run_json_raw", &mgProgramRunJSONAddr},
		{"mg_progress_snapshot", &mgProgressSnapshotAddr},
		{"mg_progress_resume_return_raw", &mgProgressResumeReturnRawAddr},
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

func stringRefs(values []string) []Str {
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
// string, freeing the source. The intermediate copy + unsafe.String avoids
// the second allocation that string(unsafe.Slice(...)) would incur.
func TakeString(buf Bytes) string {
	if buf.Ptr == nil {
		return ""
	}
	defer mgBytesFree(buf.Ptr, buf.Len)
	if buf.Len == 0 {
		return ""
	}
	bytes := make([]byte, buf.Len)
	copy(bytes, unsafe.Slice((*byte)(buf.Ptr), buf.Len))
	value := unsafe.String(unsafe.SliceData(bytes), len(bytes))
	runtime.KeepAlive(bytes)
	return value
}

// RawValueFree releases any Rust-owned payload referenced by a RawValue.
// Safe to call on a zero value or nil pointer.
func RawValueFree(value *RawValue) {
	if value != nil {
		mgRawValueFree(ptrOf(value))
	}
}

// TakeError consumes a Rust-owned error handle and converts it to a Go
// *Error, freeing the handle. Returns nil when handle == 0.
func TakeError(handle uintptr) error {
	if handle == 0 {
		return nil
	}
	defer mgErrorFree(handle)
	var typ, message, display Bytes
	_ = mgErrorType(handle, ptrOf(&typ))
	_ = mgErrorMessage(handle, ptrOf(&message))
	_ = mgErrorDisplay(handle, ptrOf(&display))
	return &Error{
		Type:    TakeString(typ),
		Message: TakeString(message),
		Display: TakeString(display),
	}
}

// TypeCheck runs Rust-side type checking for code and optional stubs.
func TypeCheck(code, scriptName, stubs, stubsName string) error {
	if err := EnsureLoaded(); err != nil {
		return err
	}
	var errHandle uintptr
	codePtr, codeLen := stringArgs(code)
	scriptPtr, scriptLen := stringArgs(scriptName)
	stubsPtr, stubsLen := stringArgs(stubs)
	stubsNamePtr, stubsNameLen := stringArgs(stubsName)
	status := mgTypeCheck(codePtr, codeLen, scriptPtr, scriptLen, stubsPtr, stubsLen, stubsNamePtr, stubsNameLen, uintptr(ptrOf(&errHandle)))
	runtime.KeepAlive(&errHandle)
	if status != StatusOK {
		return TakeError(errHandle)
	}
	return nil
}

// MountNew creates a Rust-side mount handle.
func MountNew(virtualPath, hostPath string, mode uint32, writeBytesLimit *uint64) (uintptr, error) {
	if err := EnsureLoaded(); err != nil {
		return 0, err
	}
	args := MountNewArgs{
		VirtualPath: StringRef(virtualPath),
		HostPath:    StringRef(hostPath),
		Mode:        mode,
	}
	if writeBytesLimit != nil {
		args.HasWriteBytesLimit = 1
		args.WriteBytesLimit = *writeBytesLimit
	}
	var handle, errHandle uintptr
	status := mgMountNew(ptrOf(&args), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// MountFree releases a Rust-side mount handle.
func MountFree(handle uintptr) {
	if handle != 0 {
		mgMountFree(handle)
	}
}

// MountHandleOSCall asks the mounted filesystem layer to handle one OS call.
func MountHandleOSCall(mounts []uintptr, function string, args, kwargKeys, kwargValues []uintptr) (uintptr, bool, error) {
	if err := EnsureLoaded(); err != nil {
		return 0, false, err
	}
	if len(kwargKeys) != len(kwargValues) {
		return 0, false, errors.New("monty: kwarg keys and values have different lengths")
	}
	callArgs := MountCallArgs{
		Mounts:      slicePointer(mounts),
		MountCount:  uintptr(len(mounts)),
		Function:    StringRef(function),
		Args:        slicePointer(args),
		ArgCount:    uintptr(len(args)),
		KwargKeys:   slicePointer(kwargKeys),
		KwargValues: slicePointer(kwargValues),
		KwargCount:  uintptr(len(kwargKeys)),
	}
	var out MountOutput
	status := mgMountHandleOSCall(ptrOf(&callArgs), ptrOf(&out))
	if status != StatusOK {
		return 0, out.Handled != 0, TakeError(out.Error)
	}
	return out.Value, out.Handled != 0, nil
}

// ReplNew creates a Rust-side REPL handle.
func ReplNew(scriptName string, limits *Limits) (uintptr, error) {
	if err := EnsureLoaded(); err != nil {
		return 0, err
	}
	args := ReplNewArgs{ScriptName: StringRef(scriptName)}
	if limits != nil {
		args.Limits = ptrOf(limits)
	}
	var handle, errHandle uintptr
	status := mgReplNew(ptrOf(&args), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ReplFree releases a Rust-side REPL handle.
func ReplFree(handle uintptr) {
	if handle != 0 {
		mgReplFree(handle)
	}
}

// ReplDump serializes a REPL handle into a Rust-owned snapshot buffer.
func ReplDump(handle uintptr) ([]byte, error) {
	var buffer Bytes
	var errHandle uintptr
	status := mgReplDump(handle, ptrOf(&buffer), ptrOf(&errHandle))
	if status != StatusOK {
		return nil, TakeError(errHandle)
	}
	return TakeBytes(buffer), nil
}

// ReplLoad restores a REPL handle from a snapshot created by ReplDump.
func ReplLoad(snapshot []byte) (uintptr, error) {
	if err := EnsureLoaded(); err != nil {
		return 0, err
	}
	snapshotPtr, snapshotLen := BytesRef(snapshot)
	var handle, errHandle uintptr
	status := mgReplLoad(snapshotPtr, snapshotLen, ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ReplFeedRun executes code in an existing REPL handle.
func ReplFeedRun(repl uintptr, code string, inputNames []string, inputValues []uintptr) (uintptr, string, error) {
	if len(inputNames) != len(inputValues) {
		return 0, "", errors.New("monty: REPL input names and values have different lengths")
	}
	names := stringRefs(inputNames)
	args := ReplFeedRunArgs{
		Code:        StringRef(code),
		InputNames:  slicePointer(names),
		InputValues: slicePointer(inputValues),
		InputCount:  uintptr(len(names)),
	}
	var out RunOutput
	status := mgReplFeedRun(repl, ptrOf(&args), ptrOf(&out))
	if status != StatusOK {
		return 0, TakeString(out.Print), TakeError(out.Error)
	}
	return out.Value, TakeString(out.Print), nil
}

// ReplCallFunction calls a named Python function in an existing REPL handle.
func ReplCallFunction(repl uintptr, name string, args []uintptr) (uintptr, string, error) {
	callArgs := ReplCallArgs{
		Name:     StringRef(name),
		Args:     slicePointer(args),
		ArgCount: uintptr(len(args)),
	}
	var out RunOutput
	status := mgReplCallFunction(repl, ptrOf(&callArgs), ptrOf(&out))
	if status != StatusOK {
		return 0, TakeString(out.Print), TakeError(out.Error)
	}
	return out.Value, TakeString(out.Print), nil
}

// ReplFunctionNames returns Python function names defined in the REPL.
func ReplFunctionNames(repl uintptr) ([]string, error) {
	var handle, errHandle uintptr
	status := mgReplFunctionNames(repl, ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return nil, TakeError(errHandle)
	}
	defer ValueFree(handle)
	return stringListFromValue(handle)
}

// ReplHasFunction reports whether the REPL contains a named Python function.
func ReplHasFunction(repl uintptr, name string) bool {
	namePtr, nameLen := stringArgs(name)
	return mgReplHasFunction(repl, namePtr, nameLen) != 0
}

// ReplContinuationMode returns the parser continuation mode for interactive code.
func ReplContinuationMode(code string) (uint32, error) {
	if err := EnsureLoaded(); err != nil {
		return 0, err
	}
	codePtr, codeLen := stringArgs(code)
	return mgReplContinuationMode(codePtr, codeLen), nil
}

// ProgramCompile compiles code into a Rust-side program handle.
func ProgramCompile(code, scriptName string, inputNames []string) (uintptr, error) {
	if err := EnsureLoaded(); err != nil {
		return 0, err
	}
	names := stringRefs(inputNames)
	args := ProgramCompileArgs{
		Code:       StringRef(code),
		ScriptName: StringRef(scriptName),
		InputNames: slicePointer(names),
		InputCount: uintptr(len(names)),
	}
	var handle, errHandle uintptr
	status := mgProgramCompile(ptrOf(&args), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ProgramFree releases a Rust-side program handle.
func ProgramFree(handle uintptr) {
	if handle != 0 {
		mgProgramFree(handle)
	}
}

var (
	startOutputPool = sync.Pool{
		New: func() any { return new(StartOutput) },
	}
	runRawOutputPool = sync.Pool{
		New: func() any { return new(RunRawOutput) },
	}
	runJSONOutputPool = sync.Pool{
		New: func() any { return new(RunJSONOutput) },
	}
)

// ProgramStartRaw starts a program using RawValue inputs.
func ProgramStartRaw(program uintptr, inputs []RawValue, limits *Limits) (uintptr, string, error) {
	if err := EnsureLoaded(); err != nil {
		return 0, "", err
	}
	out := startOutputPool.Get().(*StartOutput) //nolint:errcheck // pool only stores *StartOutput
	*out = StartOutput{}
	defer startOutputPool.Put(out)
	status := syscall5(
		mgProgramStartRawAddr,
		program,
		sliceAddress(inputs),
		uintptr(len(inputs)),
		uintptr(ptrOf(limits)),
		uintptr(ptrOf(out)),
	)
	// The trampoline passes these as bare uintptr, so the GC does not see them
	// as live across the Rust call; pin them until it returns.
	runtime.KeepAlive(inputs)
	runtime.KeepAlive(limits)
	if status != StatusOK {
		return 0, TakeString(out.Print), TakeError(out.Error)
	}
	return out.Progress, TakeString(out.Print), nil
}

// ProgramRunRaw runs a program to completion using RawValue inputs and output.
func ProgramRunRaw(program uintptr, inputs []RawValue, limits *Limits) (RawValue, string, error) {
	if err := EnsureLoaded(); err != nil {
		return RawValue{}, "", err
	}
	out := runRawOutputPool.Get().(*RunRawOutput) //nolint:errcheck // pool only stores *RunRawOutput
	*out = RunRawOutput{}
	defer runRawOutputPool.Put(out)
	status := syscall5(
		mgProgramRunRawAddr,
		program,
		sliceAddress(inputs),
		uintptr(len(inputs)),
		uintptr(ptrOf(limits)),
		uintptr(ptrOf(out)),
	)
	runtime.KeepAlive(inputs)
	runtime.KeepAlive(limits)
	if status != StatusOK {
		return RawValue{}, TakeString(out.Print), TakeError(out.Error)
	}
	value := out.Value
	out.Value = RawValue{}
	return value, TakeString(out.Print), nil
}

// ProgramRunHostRaw runs a program with a direct host-function callback.
func ProgramRunHostRaw(program uintptr, inputs []RawValue, limits *Limits, names []Str, callback uintptr, userData uintptr) (RawValue, string, error) {
	if err := EnsureLoaded(); err != nil {
		return RawValue{}, "", err
	}
	out := runRawOutputPool.Get().(*RunRawOutput) //nolint:errcheck // pool only stores *RunRawOutput
	*out = RunRawOutput{}
	defer runRawOutputPool.Put(out)
	status := syscall9(
		mgProgramRunHostRawAddr,
		program,
		sliceAddress(inputs),
		uintptr(len(inputs)),
		uintptr(ptrOf(limits)),
		sliceAddress(names),
		uintptr(len(names)),
		callback,
		userData,
		uintptr(ptrOf(out)),
	)
	runtime.KeepAlive(inputs)
	runtime.KeepAlive(limits)
	// names holds Str values whose Ptr fields point at Go strings; keeping the
	// backing array alive transitively pins those strings across the call.
	runtime.KeepAlive(names)
	if status != StatusOK {
		return RawValue{}, TakeString(out.Print), TakeError(out.Error)
	}
	value := out.Value
	out.Value = RawValue{}
	return value, TakeString(out.Print), nil
}

// resetFastOutput clears every header field that the Rust side writes back
// without touching the 8 KiB Scratch payload. Zeroing the full struct each
// call would memset all of Scratch even when no flat payload follows.
func resetFastOutput(out *RunFastOutput) {
	out.Format = 0
	out.BytesInScratch = 0
	out.Value = RawValue{}
	out.Bytes = Bytes{}
	out.Print = Bytes{}
	out.Error = 0
}

// ProgramCompileRunFastRaw compiles a program, runs it once, and frees it in
// a single FFI hop. The caller owns out and is responsible for releasing any
// handles or buffers (Value, Bytes) it contains.
func ProgramCompileRunFastRaw(args *CompileRunFastRawArgs, out *RunFastOutput) (string, error) {
	if err := EnsureLoaded(); err != nil {
		return "", err
	}
	resetFastOutput(out)
	status := syscall2(
		mgProgramCompileRunFastAddr,
		uintptr(ptrOf(args)),
		uintptr(ptrOf(out)),
	)
	// Keeping args alive transitively pins everything it references: the Code
	// string, the InputNames/InputValues backing arrays, and Limits.
	runtime.KeepAlive(args)
	if status != StatusOK {
		return TakeString(out.Print), TakeError(out.Error)
	}
	return TakeString(out.Print), nil
}

// ProgramRunFastRaw runs a program using Rust's fastest selected raw output
// format. The caller owns out and is responsible for releasing any handles or
// buffers (Value, Bytes) it contains.
func ProgramRunFastRaw(program uintptr, inputs []RawValue, limits *Limits, out *RunFastOutput) (string, error) {
	if err := EnsureLoaded(); err != nil {
		return "", err
	}
	resetFastOutput(out)
	status := syscall5(
		mgProgramRunFastAddr,
		program,
		sliceAddress(inputs),
		uintptr(len(inputs)),
		uintptr(ptrOf(limits)),
		uintptr(ptrOf(out)),
	)
	runtime.KeepAlive(inputs)
	runtime.KeepAlive(limits)
	if status != StatusOK {
		return TakeString(out.Print), TakeError(out.Error)
	}
	return TakeString(out.Print), nil
}

// ProgramRunJSONRaw runs a program and returns its JSON bytes.
func ProgramRunJSONRaw(program uintptr, inputs []RawValue, limits *Limits) ([]byte, string, error) {
	if err := EnsureLoaded(); err != nil {
		return nil, "", err
	}
	out := runJSONOutputPool.Get().(*RunJSONOutput) //nolint:errcheck // pool only stores *RunJSONOutput
	*out = RunJSONOutput{}
	defer runJSONOutputPool.Put(out)
	status := syscall5(
		mgProgramRunJSONAddr,
		program,
		sliceAddress(inputs),
		uintptr(len(inputs)),
		uintptr(ptrOf(limits)),
		uintptr(ptrOf(out)),
	)
	runtime.KeepAlive(inputs)
	runtime.KeepAlive(limits)
	if status != StatusOK {
		return nil, TakeString(out.Print), TakeError(out.Error)
	}
	return TakeBytes(out.Value), TakeString(out.Print), nil
}

// ProgramDump serializes a program handle.
func ProgramDump(program uintptr) ([]byte, error) {
	var buffer Bytes
	var errHandle uintptr
	status := mgProgramDump(program, ptrOf(&buffer), ptrOf(&errHandle))
	if status != StatusOK {
		return nil, TakeError(errHandle)
	}
	return TakeBytes(buffer), nil
}

// ProgramCode returns the source code stored in a program handle.
func ProgramCode(program uintptr) (string, error) {
	var buffer Bytes
	var errHandle uintptr
	status := mgProgramCode(program, ptrOf(&buffer), ptrOf(&errHandle))
	if status != StatusOK {
		return "", TakeError(errHandle)
	}
	return TakeString(buffer), nil
}

// ProgramScriptName returns the script name stored in a program handle.
func ProgramScriptName(program uintptr) (string, error) {
	var buffer Bytes
	var errHandle uintptr
	status := mgProgramScriptName(program, ptrOf(&buffer), ptrOf(&errHandle))
	if status != StatusOK {
		return "", TakeError(errHandle)
	}
	return TakeString(buffer), nil
}

// ProgramInputNames returns the ordered input names stored in a program handle.
func ProgramInputNames(program uintptr) ([]string, error) {
	var handle, errHandle uintptr
	status := mgProgramInputNames(program, ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return nil, TakeError(errHandle)
	}
	defer ValueFree(handle)
	return stringListFromValue(handle)
}

// ProgramLoad restores a program handle from a snapshot created by ProgramDump.
func ProgramLoad(snapshot []byte) (uintptr, error) {
	if err := EnsureLoaded(); err != nil {
		return 0, err
	}
	snapshotPtr, snapshotLen := BytesRef(snapshot)
	var programHandle, errHandle uintptr
	status := mgProgramLoad(snapshotPtr, snapshotLen, ptrOf(&programHandle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return programHandle, nil
}

// ValueFree releases a Rust-side value handle.
func ValueFree(handle uintptr) {
	if handle != 0 {
		mgValueFree(handle)
	}
}

// ValueJSON serializes a value handle to JSON bytes.
func ValueJSON(handle uintptr) ([]byte, error) {
	var buffer Bytes
	var errHandle uintptr
	status := mgValueJSON(handle, ptrOf(&buffer), ptrOf(&errHandle))
	if status != StatusOK {
		return nil, TakeError(errHandle)
	}
	return TakeBytes(buffer), nil
}

// ValueNone creates a Python None value handle.
func ValueNone() uintptr { return mgValueNone() }

// ValueEllipsis creates a Python Ellipsis value handle.
func ValueEllipsis() uintptr { return mgValueEllipsis() }

// ValueBool creates a Python bool value handle.
func ValueBool(v bool) uintptr { return mgValueBool(boolByte(v)) }

// ValueInt creates a Python int value handle from an int64.
func ValueInt(v int64) uintptr { return mgValueInt(v) }

// ValueFloat creates a Python float value handle.
func ValueFloat(v float64) uintptr { return mgValueFloat(v) }

// ValueKind returns the RawValue kind discriminant for a value handle.
func ValueKind(v uintptr) uint32 { return mgValueKind(v) }

// ValueBoolGet extracts a bool payload from a value handle.
func ValueBoolGet(v uintptr) bool { return mgValueBoolGet(v) != 0 }

// ValueIntGet extracts an int64 payload from a value handle.
func ValueIntGet(v uintptr) int64 { return mgValueIntGet(v) }

// ValueFloatGet extracts a float64 payload from a value handle.
func ValueFloatGet(v uintptr) float64 { return mgValueFloatGet(v) }

// ValueLen returns the item count for sequence and mapping value handles.
func ValueLen(v uintptr) uintptr { return mgValueLen(v) }

// ValueItemsRaw copies sequence items into out as RawValue values.
func ValueItemsRaw(v uintptr, out []RawValue) error {
	if status := mgValueItemsRaw(v, slicePointer(out), uintptr(len(out))); status != StatusOK {
		return errors.New("monty: value is not a sequence")
	}
	return nil
}

// ValuePairsRaw copies dict pairs into out as RawPair values.
func ValuePairsRaw(v uintptr, out []RawPair) error {
	if status := mgValuePairsRaw(v, slicePointer(out), uintptr(len(out))); status != StatusOK {
		return errors.New("monty: value is not a dict")
	}
	return nil
}

// ValueBigInt creates a Python int value handle from a base-10 string.
func ValueBigInt(v string) (uintptr, error) {
	var handle, errHandle uintptr
	valuePtr, valueLen := stringArgs(v)
	status := mgValueBigInt(valuePtr, valueLen, ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueString creates a Python str value handle.
func ValueString(v string) (uintptr, error) {
	var handle, errHandle uintptr
	valuePtr, valueLen := stringArgs(v)
	status := mgValueString(valuePtr, valueLen, ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValuePath creates a Python pathlib-style path value handle.
func ValuePath(v string) (uintptr, error) {
	var handle, errHandle uintptr
	valuePtr, valueLen := stringArgs(v)
	status := mgValuePath(valuePtr, valueLen, ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueBytes creates a Python bytes value handle.
func ValueBytes(v []byte) (uintptr, error) {
	valuePtr, valueLen := BytesRef(v)
	var handle, errHandle uintptr
	status := mgValueBytes(valuePtr, valueLen, ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueListRaw creates a Python list handle from raw values.
func ValueListRaw(values []RawValue) (uintptr, error) {
	var handle, errHandle uintptr
	status := mgValueListRaw(slicePointer(values), uintptr(len(values)), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueTupleRaw creates a Python tuple handle from raw values.
func ValueTupleRaw(values []RawValue) (uintptr, error) {
	var handle, errHandle uintptr
	status := mgValueTupleRaw(slicePointer(values), uintptr(len(values)), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueNamedTupleRaw creates a Python namedtuple-like handle from raw values.
func ValueNamedTupleRaw(typeName string, fieldNames []string, values []RawValue) (uintptr, error) {
	if len(fieldNames) != len(values) {
		return 0, errors.New("monty: named tuple field names and values have different lengths")
	}
	namePtr, nameLen := stringArgs(typeName)
	names := stringRefs(fieldNames)
	var handle, errHandle uintptr
	status := mgValueNamedTupleRaw(namePtr, nameLen, slicePointer(names), slicePointer(values), uintptr(len(values)), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueSetRaw creates a Python set handle from raw values.
func ValueSetRaw(values []RawValue) (uintptr, error) {
	var handle, errHandle uintptr
	status := mgValueSetRaw(slicePointer(values), uintptr(len(values)), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueFrozenSetRaw creates a Python frozenset handle from raw values.
func ValueFrozenSetRaw(values []RawValue) (uintptr, error) {
	var handle, errHandle uintptr
	status := mgValueFrozenSetRaw(slicePointer(values), uintptr(len(values)), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueDictRaw creates a Python dict handle from raw key/value pairs.
func ValueDictRaw(pairs []RawPair) (uintptr, error) {
	var handle, errHandle uintptr
	status := mgValueDictRaw(slicePointer(pairs), uintptr(len(pairs)), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueDate creates a Python date handle.
func ValueDate(value Date) (uintptr, error) {
	var handle, errHandle uintptr
	status := mgValueDate(ptrOf(&value), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueDateTime creates a Python datetime handle.
func ValueDateTime(value DateTime) (uintptr, error) {
	var handle, errHandle uintptr
	status := mgValueDateTime(ptrOf(&value), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueTimeDelta creates a Python timedelta handle.
func ValueTimeDelta(value TimeDelta) (uintptr, error) {
	var handle, errHandle uintptr
	status := mgValueTimeDelta(ptrOf(&value), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueTimeZone creates a Python timezone handle.
func ValueTimeZone(value TimeZone) (uintptr, error) {
	var handle, errHandle uintptr
	status := mgValueTimeZone(ptrOf(&value), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueDataclassRaw creates a Python dataclass-like handle from raw pairs.
func ValueDataclassRaw(name string, typeID uint64, fieldNames []string, attrs []RawPair, frozen bool) (uintptr, error) {
	names := stringRefs(fieldNames)
	var handle, errHandle uintptr
	args := DataclassRawArgs{
		Name:       StringRef(name),
		TypeID:     typeID,
		FieldNames: slicePointer(names),
		FieldCount: uintptr(len(names)),
		Attrs:      slicePointer(attrs),
		AttrCount:  uintptr(len(attrs)),
		Frozen:     boolByte(frozen),
	}
	status := mgValueDataclassRaw(ptrOf(&args), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueFunction creates a Python external-function marker handle.
func ValueFunction(name, doc string) (uintptr, error) {
	var handle, errHandle uintptr
	namePtr, nameLen := stringArgs(name)
	docPtr, docLen := stringArgs(doc)
	status := mgValueFunction(namePtr, nameLen, docPtr, docLen, ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueException creates a Python exception value handle.
func ValueException(excType, message string) (uintptr, error) {
	var handle, errHandle uintptr
	excPtr, excLen := stringArgs(excType)
	msgPtr, msgLen := stringArgs(message)
	status := mgValueException(excPtr, excLen, msgPtr, msgLen, ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueExceptionType returns the exception type name from a value handle.
func ValueExceptionType(v uintptr) (string, error) {
	var text Bytes
	var errHandle uintptr
	status := mgValueExceptionType(v, ptrOf(&text), ptrOf(&errHandle))
	if status != StatusOK {
		return "", TakeError(errHandle)
	}
	return TakeString(text), nil
}

// ValueExceptionMessage returns the exception message from a value handle.
func ValueExceptionMessage(v uintptr) (string, error) {
	var text Bytes
	var errHandle uintptr
	status := mgValueExceptionMessage(v, ptrOf(&text), ptrOf(&errHandle))
	if status != StatusOK {
		return "", TakeError(errHandle)
	}
	return TakeString(text), nil
}

// ValueText returns the textual payload or representation for a value handle.
func ValueText(v uintptr) (string, error) {
	var text Bytes
	var errHandle uintptr
	status := mgValueText(v, ptrOf(&text), ptrOf(&errHandle))
	if status != StatusOK {
		return "", TakeError(errHandle)
	}
	return TakeString(text), nil
}

// ValueBytesGet returns the byte payload from a bytes value handle.
func ValueBytesGet(v uintptr) ([]byte, error) {
	var bytes Bytes
	var errHandle uintptr
	status := mgValueBytesGet(v, ptrOf(&bytes), ptrOf(&errHandle))
	if status != StatusOK {
		return nil, TakeError(errHandle)
	}
	return TakeBytes(bytes), nil
}

// ValueDateGet returns the date payload from a value handle.
func ValueDateGet(v uintptr) (Date, error) {
	var value Date
	var errHandle uintptr
	status := mgValueDateGet(v, ptrOf(&value), ptrOf(&errHandle))
	if status != StatusOK {
		return Date{}, TakeError(errHandle)
	}
	return value, nil
}

// ValueDateTimeGet returns the datetime payload from a value handle.
func ValueDateTimeGet(v uintptr) (DateTime, error) {
	var value DateTime
	var errHandle uintptr
	status := mgValueDateTimeGet(v, ptrOf(&value), ptrOf(&errHandle))
	if status != StatusOK {
		return DateTime{}, TakeError(errHandle)
	}
	return value, nil
}

// ValueTimeDeltaGet returns the timedelta payload from a value handle.
func ValueTimeDeltaGet(v uintptr) (TimeDelta, error) {
	var value TimeDelta
	var errHandle uintptr
	status := mgValueTimeDeltaGet(v, ptrOf(&value), ptrOf(&errHandle))
	if status != StatusOK {
		return TimeDelta{}, TakeError(errHandle)
	}
	return value, nil
}

// ValueTimeZoneGet returns the timezone payload from a value handle.
func ValueTimeZoneGet(v uintptr) (TimeZone, error) {
	var value TimeZone
	var errHandle uintptr
	status := mgValueTimeZoneGet(v, ptrOf(&value), ptrOf(&errHandle))
	if status != StatusOK {
		return TimeZone{}, TakeError(errHandle)
	}
	return value, nil
}

// ValueNamedTupleTypeName returns the type name from a namedtuple value handle.
func ValueNamedTupleTypeName(v uintptr) (string, error) {
	var text Bytes
	var errHandle uintptr
	status := mgValueNamedTupleTypeName(v, ptrOf(&text), ptrOf(&errHandle))
	if status != StatusOK {
		return "", TakeError(errHandle)
	}
	return TakeString(text), nil
}

// ValueNamedTupleFieldNames returns field names from a namedtuple value handle.
func ValueNamedTupleFieldNames(v uintptr) ([]string, error) {
	var handle, errHandle uintptr
	status := mgValueNamedTupleFieldNames(v, ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return nil, TakeError(errHandle)
	}
	defer ValueFree(handle)
	return stringListFromValue(handle)
}

// ValueDataclassName returns the class name from a dataclass value handle.
func ValueDataclassName(v uintptr) (string, error) {
	var text Bytes
	var errHandle uintptr
	status := mgValueDataclassName(v, ptrOf(&text), ptrOf(&errHandle))
	if status != StatusOK {
		return "", TakeError(errHandle)
	}
	return TakeString(text), nil
}

// ValueDataclassTypeID returns the type ID from a dataclass value handle.
func ValueDataclassTypeID(v uintptr) uint64 { return mgValueDataclassTypeID(v) }

// ValueDataclassFieldNames returns field names from a dataclass value handle.
func ValueDataclassFieldNames(v uintptr) ([]string, error) {
	var handle, errHandle uintptr
	status := mgValueDataclassFieldNames(v, ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return nil, TakeError(errHandle)
	}
	defer ValueFree(handle)
	return stringListFromValue(handle)
}

// ValueDataclassFrozen reports whether a dataclass value handle is frozen.
func ValueDataclassFrozen(v uintptr) bool { return mgValueDataclassFrozen(v) != 0 }

func stringListFromValue(handle uintptr) ([]string, error) {
	count := int(ValueLen(handle))
	if count == 0 {
		return []string{}, nil
	}
	rawItems := make([]RawValue, count)
	if err := ValueItemsRaw(handle, rawItems); err != nil {
		return nil, err
	}
	names := make([]string, count)
	for i := range rawItems {
		if rawItems[i].Kind != KindString {
			for j := i; j < len(rawItems); j++ {
				RawValueFree(&rawItems[j])
			}
			return nil, fmt.Errorf("monty: field name %d is %d, not string", i, rawItems[i].Kind)
		}
		names[i] = TakeString(Bytes{Ptr: rawItems[i].Ptr, Len: rawItems[i].Len})
		rawItems[i].Ptr = nil
		rawItems[i].Len = 0
	}
	return names, nil
}

// ProgressFree releases a Rust-side progress handle.
func ProgressFree(handle uintptr) {
	if handle != 0 {
		mgProgressFree(handle)
	}
}

// ProgressSnapshotGet returns a zero-copy snapshot of a progress handle.
func ProgressSnapshotGet(handle uintptr) (ProgressSnapshot, error) {
	var snapshot ProgressSnapshot
	status := syscall2(
		mgProgressSnapshotAddr,
		handle,
		uintptr(ptrOf(&snapshot)),
	)
	runtime.KeepAlive(&snapshot)
	if status != StatusOK {
		return snapshot, TakeError(snapshot.Error)
	}
	return snapshot, nil
}

// ProgressPendingLen returns the number of pending future call IDs.
func ProgressPendingLen(handle uintptr) uintptr { return mgProgressPendingLen(handle) }

// ProgressPendingID returns the pending future call ID at index i.
func ProgressPendingID(handle uintptr, i uintptr) uint32 { return mgProgressPendingID(handle, i) }

// ProgressResumePending resumes a function-call progress as a pending future.
func ProgressResumePending(progress uintptr) (uintptr, string, error) {
	var out ProgressOutput
	status := mgProgressResumePending(progress, ptrOf(&out))
	if status != StatusOK {
		return 0, TakeString(out.Print), TakeError(out.Error)
	}
	return out.Progress, TakeString(out.Print), nil
}

// ProgressResumeReturnRaw resumes a progress handle with a raw value.
func ProgressResumeReturnRaw(progress uintptr, value *RawValue) (uintptr, string, error) {
	var out ProgressOutput
	status := syscall3(
		mgProgressResumeReturnRawAddr,
		progress,
		uintptr(ptrOf(value)),
		uintptr(ptrOf(&out)),
	)
	runtime.KeepAlive(value)
	if status != StatusOK {
		return 0, TakeString(out.Print), TakeError(out.Error)
	}
	return out.Progress, TakeString(out.Print), nil
}

// ProgressResumeException resumes a progress handle by raising an exception.
func ProgressResumeException(progress uintptr, excType, message string) (uintptr, string, error) {
	var out ProgressOutput
	excPtr, excLen := stringArgs(excType)
	msgPtr, msgLen := stringArgs(message)
	status := mgProgressResumeException(progress, excPtr, excLen, msgPtr, msgLen, ptrOf(&out))
	if status != StatusOK {
		return 0, TakeString(out.Print), TakeError(out.Error)
	}
	return out.Progress, TakeString(out.Print), nil
}

// ProgressResumeNameValueRaw resumes a name lookup with a raw value.
func ProgressResumeNameValueRaw(progress uintptr, value *RawValue) (uintptr, string, error) {
	var out ProgressOutput
	status := mgProgressResumeNameValueRaw(progress, ptrOf(value), ptrOf(&out))
	if status != StatusOK {
		return 0, TakeString(out.Print), TakeError(out.Error)
	}
	return out.Progress, TakeString(out.Print), nil
}

// ProgressResumeNameUndefined resumes a name lookup as undefined.
func ProgressResumeNameUndefined(progress uintptr) (uintptr, string, error) {
	var out ProgressOutput
	status := mgProgressResumeNameUndefined(progress, ptrOf(&out))
	if status != StatusOK {
		return 0, TakeString(out.Print), TakeError(out.Error)
	}
	return out.Progress, TakeString(out.Print), nil
}

// ProgressResumeFutures resumes a progress handle with resolved future results.
func ProgressResumeFutures(progress uintptr, results []FutureResult) (uintptr, string, error) {
	var out ProgressOutput
	status := mgProgressResumeFutures(progress, slicePointer(results), uintptr(len(results)), ptrOf(&out))
	if status != StatusOK {
		return 0, TakeString(out.Print), TakeError(out.Error)
	}
	return out.Progress, TakeString(out.Print), nil
}

// ProgressDump serializes a progress handle.
func ProgressDump(progress uintptr) ([]byte, error) {
	var buffer Bytes
	var errHandle uintptr
	status := mgProgressDump(progress, ptrOf(&buffer), ptrOf(&errHandle))
	if status != StatusOK {
		return nil, TakeError(errHandle)
	}
	return TakeBytes(buffer), nil
}

// ProgressLoad restores a progress handle from a snapshot created by ProgressDump.
func ProgressLoad(snapshot []byte) (uintptr, error) {
	if err := EnsureLoaded(); err != nil {
		return 0, err
	}
	snapshotPtr, snapshotLen := BytesRef(snapshot)
	var progressHandle, errHandle uintptr
	status := mgProgressLoad(snapshotPtr, snapshotLen, ptrOf(&progressHandle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return progressHandle, nil
}

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
