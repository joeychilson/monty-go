package monty

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
	"sync"
	"unsafe"

	"github.com/joeychilson/monty/internal/ffi"
)

// Program is a compiled Monty Python program.
//
// A Program may be reused for many runs. Call Close when it is no longer
// needed to release the Rust-side program handle.
type Program struct {
	mu         sync.RWMutex
	handle     uintptr
	code       string
	scriptName string
	inputs     []string
	functions  map[string]*Function
	funcNames  []ffi.Str
	cleanup    runtime.Cleanup
}

var runFastOutputPool = sync.Pool{
	New: func() any { return new(ffi.RunFastOutput) },
}

var hostCallbackPtr = ffi.NewCallback(hostFunctionCallback)

// Compile compiles Python code into a reusable Program.
func Compile(code string, opts ...CompileOption) (*Program, error) {
	config := compileConfig{scriptName: "main.py", autoStubs: true}
	for _, opt := range opts {
		opt(&config)
	}
	functions := make(map[string]*Function, len(config.functions))
	for _, function := range config.functions {
		if _, exists := functions[function.Name()]; exists {
			return nil, fmt.Errorf("monty: duplicate function %q", function.Name())
		}
		functions[function.Name()] = function
	}
	if config.typeCheck {
		stubs := config.typeStubs
		if config.autoStubs {
			stubs = joinStubs(stubs, functions)
		}
		if err := ffi.TypeCheck(code, config.scriptName, stubs, "type_stubs.pyi"); err != nil {
			return nil, normalizeError(err)
		}
	}
	handle, err := ffi.ProgramCompile(code, config.scriptName, config.inputs)
	if err != nil {
		return nil, normalizeError(err)
	}
	program := &Program{
		handle:     handle,
		code:       code,
		scriptName: config.scriptName,
		inputs:     slices.Clone(config.inputs),
		functions:  functions,
		funcNames:  hostFunctionNameRefs(functions),
	}
	program.cleanup = runtime.AddCleanup(program, ffi.ProgramFree, handle)
	return program, nil
}

// LoadProgram restores a Program created by Program.Dump.
func LoadProgram(snapshot []byte) (*Program, error) {
	handle, err := ffi.ProgramLoad(snapshot)
	if err != nil {
		return nil, normalizeError(err)
	}
	code, err := ffi.ProgramCode(handle)
	if err != nil {
		ffi.ProgramFree(handle)
		return nil, normalizeError(err)
	}
	scriptName, err := ffi.ProgramScriptName(handle)
	if err != nil {
		ffi.ProgramFree(handle)
		return nil, normalizeError(err)
	}
	inputs, err := ffi.ProgramInputNames(handle)
	if err != nil {
		ffi.ProgramFree(handle)
		return nil, normalizeError(err)
	}
	program := &Program{
		handle:     handle,
		code:       code,
		scriptName: scriptName,
		inputs:     inputs,
		functions:  map[string]*Function{},
	}
	program.cleanup = runtime.AddCleanup(program, ffi.ProgramFree, handle)
	return program, nil
}

// CompileAndRun compiles code, runs it once with inputs, and frees the
// underlying Rust program — all in a single FFI hop. It is materially faster
// than Compile + Run + Close for one-shot evaluations because it pays the
// cgocall trampoline cost once instead of three times.
//
// The inputs argument is normalized through the same rules as RunAs. The script
// name used in tracebacks is fixed to "main.py" (compileAndRunScriptName); use
// Compile + Run if you need to brand it.
//
// Cancellation behaves as documented on Program.Run: a ctx deadline bounds a
// runaway snippet, but plain cancellation only takes effect at progress-loop
// boundaries.
// compileAndRunScriptName is the traceback filename CompileAndRun uses on both
// its fast single-hop path and its dispatch-loop fallback, kept in one place so
// the two cannot drift.
const compileAndRunScriptName = "main.py"

func CompileAndRun(ctx context.Context, code string, inputs any, opts ...RunOption) (Value, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Value{}, err
	}
	values, err := normalizeInputs(inputs)
	if err != nil {
		return Value{}, err
	}
	var config runConfig
	for _, opt := range opts {
		opt(&config)
	}
	if deadline, ok := ctx.Deadline(); ok {
		config.limits = limitsWithContextDeadline(config.limits, deadline)
	}
	if config.needsDispatchLoop() {
		inputNames := make([]string, 0, len(values))
		for name := range values {
			inputNames = append(inputNames, name)
		}
		program, err := Compile(code, WithScriptName(compileAndRunScriptName), WithInputs(inputNames...))
		if err != nil {
			return Value{}, err
		}
		defer program.Close()
		return program.Run(ctx, values, opts...)
	}
	var arena rawArena
	nameRefs := make([]ffi.Str, 0, len(values))
	raw := make([]ffi.RawValue, 0, len(values))
	// Inputs whose Kind has no inline raw form (Date, DateTime, Path,
	// NamedTuple, Dataclass, ...) are converted to owned Rust handles, which
	// arena.ownsHandles records. Rust's read_raw_value consumes those handles
	// in place on a successful read — nulling each slot — so freeing here is a
	// no-op on success but reclaims handles Rust never read: when valueToRaw
	// fails partway through this loop, and (the real leak) when compilation
	// fails before the inputs are read at all. Mirrors Program.rawInputs.
	defer func() {
		if arena.ownsHandles {
			freeOwnedRawValues(raw)
		}
	}()
	for name, value := range values {
		rawInput, err := valueToRaw(value, &arena)
		if err != nil {
			return Value{}, err
		}
		nameRefs = append(nameRefs, ffi.StringRef(name))
		raw = append(raw, rawInput)
	}
	var nameRefsPtr, rawPtr unsafe.Pointer
	if len(nameRefs) > 0 {
		nameRefsPtr = unsafe.Pointer(unsafe.SliceData(nameRefs))
		rawPtr = unsafe.Pointer(unsafe.SliceData(raw))
	}
	var ffiLimits *ffi.Limits
	if config.limits != nil {
		ffiLimits = config.limits.ffi()
	}
	args := ffi.CompileRunFastRawArgs{
		Code:            ffi.StringRef(code),
		ScriptName:      ffi.StringRef(compileAndRunScriptName),
		InputNames:      nameRefsPtr,
		InputCount:      uintptr(len(nameRefs)),
		InputValues:     rawPtr,
		InputValueCount: uintptr(len(raw)),
		Limits:          ffiLimits,
	}
	output := runFastOutputPool.Get().(*ffi.RunFastOutput) //nolint:errcheck // pool only stores *ffi.RunFastOutput
	defer runFastOutputPool.Put(output)
	printed, err := ffi.ProgramCompileRunFastRaw(&args, output)
	runtime.KeepAlive(values)
	writeErr := writePrinted(config.stdout, printed)
	if err != nil {
		ffi.RawValueFree(&output.Value)
		freeFastOutputBytes(output)
		return Value{}, errors.Join(normalizeError(err), writeErr)
	}
	if writeErr != nil {
		ffi.RawValueFree(&output.Value)
		freeFastOutputBytes(output)
		return Value{}, writeErr
	}
	return decodeFastOutput(output)
}

// freeFastOutputBytes releases the Rust-owned flat buffer when one was emitted.
// Outputs whose bytes were copied into the inline scratch require no FFI
// call; the buffer lives entirely on the Go side.
func freeFastOutputBytes(out *ffi.RunFastOutput) {
	if out.BytesInScratch == 0 {
		ffi.MaybeBytesFree(out.Bytes)
	}
}

func decodeFastOutput(out *ffi.RunFastOutput) (Value, error) {
	if out.Format != ffi.FastFormatFlat {
		return decodeRawValue(out.Value)
	}
	var owned []byte
	if out.Bytes.Len != 0 {
		// Decoded strings borrow from this buffer, so it must outlive the
		// returned Value tree instead of pointing at pooled scratch memory.
		owned = make([]byte, out.Bytes.Len)
		copy(owned, ffi.UnsafeBytes(out.Bytes))
	}
	freeFastOutputBytes(out)
	return decodeFlatValue(owned)
}

// CompileAndRunAs combines CompileAndRun with As[T] so callers get a typed
// result in a single line.
func CompileAndRunAs[T any](ctx context.Context, code string, inputs any, opts ...RunOption) (T, error) {
	var zero T
	value, err := CompileAndRun(ctx, code, inputs, opts...)
	if err != nil {
		return zero, err
	}
	return As[T](value)
}

// Close releases the Rust-side program handle.
//
// Close is idempotent.
func (p *Program) Close() {
	if p == nil {
		return
	}
	p.cleanup.Stop()
	p.mu.Lock()
	handle := p.handle
	p.handle = 0
	p.mu.Unlock()
	if handle != 0 {
		ffi.ProgramFree(handle)
	}
}

// Dump serializes a compiled Program so it can be loaded in another process.
func (p *Program) Dump() ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("monty: program is closed")
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.handle == 0 {
		return nil, fmt.Errorf("monty: program is closed")
	}
	snapshot, err := ffi.ProgramDump(p.handle)
	return snapshot, normalizeError(err)
}

// Code returns the source code used to compile the Program.
func (p *Program) Code() string { return p.code }

// ScriptName returns the filename Monty uses in tracebacks and diagnostics.
func (p *Program) ScriptName() string { return p.scriptName }

// Inputs returns the ordered input names declared at compile time.
func (p *Program) Inputs() []string { return slices.Clone(p.inputs) }

// Start begins executing the Program and returns the first progress state.
//
// Use Start when the caller wants to manually resolve external calls,
// filesystem calls, futures, or persisted snapshots. For ordinary run-to-value
// execution, use Program.Run or RunAs.
//
// Cancellation behaves as documented on Program.Run: a ctx deadline bounds a
// runaway snippet, but plain cancellation is only checked before each step is
// resumed, not while Python runs inside a single Rust call.
func (p *Program) Start(ctx context.Context, inputs any, opts ...RunOption) (Progress, error) {
	if p == nil {
		return nil, fmt.Errorf("monty: program is closed")
	}
	return p.start(ctx, inputs, p.runConfig(opts...))
}

func (p *Program) start(ctx context.Context, inputs any, config runConfig) (Progress, error) {
	config, err := p.prepareRun(ctx, config)
	if err != nil {
		return nil, err
	}
	rawInputs, keepAlive, err := p.rawInputs(inputs)
	if err != nil {
		return nil, err
	}
	var progressHandle uintptr
	var snapshot ffi.ProgressSnapshot
	printed, err := p.callLocked(rawInputs, keepAlive, func(handle uintptr) (string, error) {
		var printed string
		progressHandle, snapshot, printed, err = ffi.ProgramStartRawSnapshot(handle, rawInputs, config.ffiLimits())
		return printed, err
	})
	// On a call error Rust did not write the snapshot or a handle, so there is
	// nothing to release here.
	if err != nil {
		writeErr := writePrinted(config.stdout, printed)
		return nil, errors.Join(normalizeError(err), writeErr)
	}
	progress, err := progressFromSnapshot(progressHandle, snapshot, config.stdout)
	if err != nil {
		return nil, err
	}
	if writeErr := writePrinted(config.stdout, printed); writeErr != nil {
		_ = progress.Close() //nolint:errcheck // releasing the just-started progress after a stdout write error
		return nil, writeErr
	}
	return progress, nil
}

func (p *Program) runDirectValue(inputs any, config runConfig) (Value, error) {
	rawInputs, keepAlive, err := p.rawInputs(inputs)
	if err != nil {
		return Value{}, err
	}
	output := runFastOutputPool.Get().(*ffi.RunFastOutput) //nolint:errcheck // pool only stores *ffi.RunFastOutput
	defer runFastOutputPool.Put(output)
	printed, err := p.callLocked(rawInputs, keepAlive, func(handle uintptr) (string, error) {
		return ffi.ProgramRunFastRaw(handle, rawInputs, config.ffiLimits(), output)
	})
	writeErr := writePrinted(config.stdout, printed)
	if err != nil || writeErr != nil {
		ffi.RawValueFree(&output.Value)
		freeFastOutputBytes(output)
		if err != nil {
			return Value{}, errors.Join(normalizeError(err), writeErr)
		}
		return Value{}, writeErr
	}
	return decodeFastOutput(output)
}

func (p *Program) runDirectRaw(inputs any, config runConfig) (ffi.RawValue, error) {
	rawInputs, keepAlive, err := p.rawInputs(inputs)
	if err != nil {
		return ffi.RawValue{}, err
	}
	var rawResult ffi.RawValue
	printed, err := p.callLocked(rawInputs, keepAlive, func(handle uintptr) (string, error) {
		var printed string
		rawResult, printed, err = ffi.ProgramRunRaw(handle, rawInputs, config.ffiLimits())
		return printed, err
	})
	writeErr := writePrinted(config.stdout, printed)
	if err != nil {
		return ffi.RawValue{}, errors.Join(normalizeError(err), writeErr)
	}
	if writeErr != nil {
		ffi.RawValueFree(&rawResult)
		return ffi.RawValue{}, writeErr
	}
	return rawResult, nil
}

// Run executes the Program until it completes and returns the final Python value.
//
// Registered Go functions are resolved automatically. Runs without registered
// functions use a direct FFI path for lower overhead.
//
// Cancellation: a ctx deadline is enforced as a hard resource limit (translated
// to Limits.MaxDuration), so it reliably bounds a runaway snippet. Plain
// cancellation (context.WithCancel) is only observed before the run starts and
// at progress-loop boundaries between Go-resolved calls; it cannot interrupt
// Python executing inside a single Rust call. To bound a snippet that may loop
// forever, use a ctx deadline or WithLimits(Limits{MaxDuration: ...}).
func (p *Program) Run(ctx context.Context, inputs any, opts ...RunOption) (Value, error) {
	if p == nil {
		return Value{}, fmt.Errorf("monty: program is closed")
	}
	return p.run(ctx, inputs, p.runConfig(opts...))
}

// run executes the program from an already-built runConfig. RunAs and RunJSON
// call it directly so options are applied exactly once instead of being rebuilt
// when they fall back to the value path.
func (p *Program) run(ctx context.Context, inputs any, config runConfig) (Value, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !config.needsDispatchLoop() {
		preparedConfig, err := p.prepareRun(ctx, config)
		if err != nil {
			return Value{}, err
		}
		return p.runDirectValue(inputs, preparedConfig)
	}
	if config.canUseFunctionCallbackFast() {
		return p.runFunctionCallback(ctx, inputs, config)
	}
	if err := config.openMounts(); err != nil {
		return Value{}, err
	}
	defer config.closeMounts()
	progress, err := p.start(ctx, inputs, config)
	if err != nil {
		return Value{}, err
	}
	return p.runProgressLoop(ctx, progress, config)
}

func (p *Program) runProgressLoop(ctx context.Context, progress Progress, config runConfig) (Value, error) {
	for {
		if err := ctx.Err(); err != nil {
			if closeErr := progress.Close(); closeErr != nil {
				return Value{}, closeErr
			}
			return Value{}, err
		}
		switch current := progress.(type) {
		case *Complete:
			return current.Value, nil
		case *NameLookup:
			function := config.functions[current.Name]
			if function == nil {
				nextProgress, err := current.ResumeUndefined(ctx)
				if err != nil {
					return Value{}, err
				}
				progress = nextProgress
				continue
			}
			nextProgress, err := current.Resume(ctx, ExternalFunction(function.Name()))
			if err != nil {
				return Value{}, err
			}
			progress = nextProgress
		case *FunctionCall:
			function := config.functions[current.Name]
			if function == nil {
				if closeErr := current.Close(); closeErr != nil {
					return Value{}, closeErr
				}
				return Value{}, fmt.Errorf("monty: external function %q called but no Go function was registered", current.Name)
			}
			result, err := function.call(ctx, current.Args, current.Kwargs)
			if err != nil {
				excType, message := exceptionFromError(err)
				nextProgress, resumeErr := current.ResumeException(ctx, excType, message)
				if resumeErr != nil {
					return Value{}, resumeErr
				}
				progress = nextProgress
				continue
			}
			nextProgress, err := current.Resume(ctx, result)
			if err != nil {
				return Value{}, err
			}
			progress = nextProgress
		case *OSCall:
			result, err := config.handleOSCall(ctx, current)
			if err != nil {
				excType, message := exceptionFromError(err)
				nextProgress, resumeErr := current.ResumeException(ctx, excType, message)
				if resumeErr != nil {
					return Value{}, resumeErr
				}
				progress = nextProgress
				continue
			}
			nextProgress, err := current.Resume(ctx, result)
			if err != nil {
				return Value{}, err
			}
			progress = nextProgress
		default:
			if closeErr := progress.Close(); closeErr != nil {
				return Value{}, closeErr
			}
			return Value{}, fmt.Errorf("monty: unsupported progress state %T in Run", progress)
		}
	}
}

type hostCallbackState struct {
	ctx       context.Context
	functions map[string]*Function
	excType   string
	message   string
}

func (p *Program) runFunctionCallback(ctx context.Context, inputs any, config runConfig) (Value, error) {
	raw, err := p.runFunctionCallbackRaw(ctx, inputs, config)
	if err != nil {
		return Value{}, err
	}
	value, err := decodeRawValue(raw)
	return value, normalizeError(err)
}

func (p *Program) runFunctionCallbackRaw(ctx context.Context, inputs any, config runConfig) (ffi.RawValue, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	config, err := p.prepareRun(ctx, config)
	if err != nil {
		return ffi.RawValue{}, err
	}
	rawInputs, keepAlive, err := p.rawInputs(inputs)
	if err != nil {
		return ffi.RawValue{}, err
	}
	names := config.functionNames
	if names == nil && len(config.functions) != 0 {
		names = hostFunctionNameRefs(config.functions)
	}
	state := &hostCallbackState{ctx: ctx, functions: config.functions}
	var rawResult ffi.RawValue
	printed, err := p.callLocked(rawInputs, keepAlive, func(handle uintptr) (string, error) {
		var printed string
		rawResult, printed, err = ffi.ProgramRunHostRaw(handle, rawInputs, config.ffiLimits(), names, hostCallbackPtr, uintptr(unsafe.Pointer(state)))
		return printed, err
	})
	runtime.KeepAlive(names)
	runtime.KeepAlive(state)
	writeErr := writePrinted(config.stdout, printed)
	if err != nil {
		return ffi.RawValue{}, errors.Join(normalizeError(err), writeErr)
	}
	if writeErr != nil {
		ffi.RawValueFree(&rawResult)
		return ffi.RawValue{}, writeErr
	}
	return rawResult, nil
}

func hostFunctionNameRefs(functions map[string]*Function) []ffi.Str {
	if len(functions) == 0 {
		return nil
	}
	names := make([]ffi.Str, 0, len(functions))
	for name := range functions {
		names = append(names, ffi.StringRef(name))
	}
	return names
}

func hostFunctionCallback(userData unsafe.Pointer, namePtr unsafe.Pointer, nameLen uintptr, argsPtr unsafe.Pointer, kwargsPtr unsafe.Pointer, outPtr unsafe.Pointer) (status int32) {
	if outPtr == nil {
		return ffi.HostCallbackException
	}
	out := (*ffi.HostFunctionOutput)(outPtr)
	*out = ffi.HostFunctionOutput{}
	if userData == nil {
		return ffi.HostCallbackException
	}
	state := (*hostCallbackState)(userData)
	// Rust calls this callback synchronously in the middle of an extern "C"
	// FFI call. A panic from user handler code (function.fastRawCall) would
	// unwind across the Rust frames, which is undefined behavior and crashes
	// the process. Convert any panic into a Python exception instead.
	defer func() {
		if r := recover(); r != nil {
			status = state.writeException(out, "RuntimeError", fmt.Sprintf("host function panicked: %v", r))
		}
	}()
	if err := state.ctx.Err(); err != nil {
		excType, message := exceptionFromError(err)
		return state.writeException(out, excType, message)
	}
	name := ""
	if namePtr != nil && nameLen != 0 {
		name = unsafe.String((*byte)(namePtr), int(nameLen))
	}
	function := state.functions[name]
	if function == nil || function.fastRawCall == nil {
		return state.writeException(out, "NameError", fmt.Sprintf("name %q is not defined", name))
	}
	if argsPtr == nil || kwargsPtr == nil {
		return state.writeException(out, "RuntimeError", "host callback args pointer is null")
	}
	args := *(*ffi.RawValue)(argsPtr)
	kwargs := *(*ffi.RawValue)(kwargsPtr)
	raw, err := function.fastRawCall(state.ctx, args, kwargs)
	if err != nil {
		excType, message := exceptionFromError(err)
		return state.writeException(out, excType, message)
	}
	out.Value = raw
	return ffi.HostCallbackReturn
}

func (s *hostCallbackState) writeException(out *ffi.HostFunctionOutput, excType, message string) int32 {
	if excType == "" {
		excType = "RuntimeError"
	}
	s.excType = excType
	s.message = message
	out.ExcType = ffi.StringRef(s.excType)
	out.Message = ffi.StringRef(s.message)
	return ffi.HostCallbackException
}

// RunAs executes program and converts the final Python value into T.
//
// Primitive Go types, structs, maps, slices, and Value are supported through
// the same conversion rules as As.
//
// Cancellation behaves as documented on Program.Run: a ctx deadline bounds a
// runaway snippet, but plain cancellation only takes effect at progress-loop
// boundaries.
func RunAs[T any](ctx context.Context, program *Program, inputs any, opts ...RunOption) (T, error) {
	var zero T
	if program == nil {
		return zero, fmt.Errorf("monty: program is closed")
	}
	config := program.runConfig(opts...)
	if !config.needsDispatchLoop() {
		return runAsDirect[T](ctx, program, inputs, config)
	}
	if config.canUseFunctionCallbackFast() {
		raw, err := program.runFunctionCallbackRaw(ctx, inputs, config)
		if err != nil {
			return zero, err
		}
		return rawAs[T](raw)
	}
	value, err := program.run(ctx, inputs, config)
	if err != nil {
		return zero, err
	}
	return As[T](value)
}

func runAsDirect[T any](ctx context.Context, program *Program, inputs any, config runConfig) (T, error) {
	var zero T
	config, err := program.prepareRun(ctx, config)
	if err != nil {
		return zero, err
	}
	rawResult, err := program.runDirectRaw(inputs, config)
	if err != nil {
		return zero, err
	}
	return rawAs[T](rawResult)
}

func rawAs[T any](raw ffi.RawValue) (T, error) {
	var zero T
	// T is narrowed by each case below; the inner assertions to T cannot fail.
	switch any(zero).(type) {
	case Value:
		value, err := decodeRawValue(raw)
		if err != nil {
			return zero, normalizeError(err)
		}
		return any(value).(T), nil //nolint:errcheck // T is Value here
	case int:
		if Kind(raw.Kind) == IntKind {
			return any(int(raw.Int)).(T), nil //nolint:errcheck // T is int here
		}
	case int64:
		if Kind(raw.Kind) == IntKind {
			return any(raw.Int).(T), nil //nolint:errcheck // T is int64 here
		}
	case float64:
		switch Kind(raw.Kind) {
		case IntKind:
			return any(float64(raw.Int)).(T), nil //nolint:errcheck // T is float64 here
		case FloatKind:
			return any(raw.Float).(T), nil //nolint:errcheck // T is float64 here
		default:
			// fall through to generic decode below
		}
	case string:
		if Kind(raw.Kind) != ExceptionKind && isStringLikeKind(Kind(raw.Kind)) {
			text := ffi.TakeString(ffi.Bytes{Ptr: raw.Ptr, Len: raw.Len})
			return any(text).(T), nil //nolint:errcheck // T is string here
		}
	case bool:
		if Kind(raw.Kind) == BoolKind {
			return any(raw.Bool != 0).(T), nil //nolint:errcheck // T is bool here
		}
	}
	value, err := decodeRawValue(raw)
	if err != nil {
		return zero, normalizeError(err)
	}
	return As[T](value)
}

func (p *Program) prepareRun(ctx context.Context, config runConfig) (runConfig, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return config, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		config.limits = limitsWithContextDeadline(config.limits, deadline)
	}
	return config, nil
}

func (p *Program) runConfig(opts ...RunOption) runConfig {
	config := runConfig{functions: p.functions, functionNames: p.funcNames}
	for _, opt := range opts {
		opt(&config)
	}
	return config
}

type rawInputKeepAlive struct {
	inputValues Inputs
	arena       rawArena
}

func (k rawInputKeepAlive) release(raw []ffi.RawValue) {
	if k.arena.ownsHandles {
		freeOwnedRawValues(raw)
	}
}

func (p *Program) rawInputs(inputs any) ([]ffi.RawValue, rawInputKeepAlive, error) {
	inputValues, err := normalizeInputs(inputs)
	if err != nil {
		return nil, rawInputKeepAlive{}, err
	}
	if len(p.inputs) == 0 {
		return nil, rawInputKeepAlive{inputValues: inputValues}, nil
	}
	var arena rawArena
	rawInputs := make([]ffi.RawValue, len(p.inputs))
	for i, name := range p.inputs {
		value, ok := inputValues[name]
		if !ok {
			err = fmt.Errorf("monty: missing input %q", name)
		} else {
			rawInputs[i], err = valueToRaw(value, &arena)
		}
		if err != nil {
			if arena.ownsHandles {
				freeOwnedRawValues(rawInputs)
			}
			return nil, rawInputKeepAlive{}, err
		}
	}
	return rawInputs, rawInputKeepAlive{inputValues: inputValues, arena: arena}, nil
}

// callLocked runs fn under the program read lock with the program handle.
// It releases input keep-alive state after fn returns and forwards fn's
// printed output and error. A zero handle short-circuits with a closed error.
func (p *Program) callLocked(rawInputs []ffi.RawValue, keepAlive rawInputKeepAlive, fn func(handle uintptr) (string, error)) (string, error) {
	p.mu.RLock()
	if p.handle == 0 {
		p.mu.RUnlock()
		keepAlive.release(rawInputs)
		return "", fmt.Errorf("monty: program is closed")
	}
	printed, err := fn(p.handle)
	p.mu.RUnlock()
	keepAlive.release(rawInputs)
	runtime.KeepAlive(keepAlive)
	return printed, err
}

func (c runConfig) ffiLimits() *ffi.Limits {
	if c.limits == nil {
		return nil
	}
	return c.limits.ffi()
}

func joinStubs(prefix string, functions map[string]*Function) string {
	parts := make([]string, 0, len(functions)+1)
	if strings.TrimSpace(prefix) != "" {
		parts = append(parts, prefix)
	}
	for _, function := range functions {
		parts = append(parts, function.PythonStub())
	}
	return strings.Join(parts, "\n\n")
}

func writePrinted(w io.Writer, printed string) error {
	if w == nil || printed == "" {
		return nil
	}
	_, err := io.WriteString(w, printed)
	return err
}
