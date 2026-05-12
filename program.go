package monty

import (
	"context"
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
}

const (
	minPooledInputCount = 1
	maxPooledInputCount = 16
)

var rawInputPools [maxPooledInputCount + 1]chan []ffi.RawValue

var runFastOutputPool = sync.Pool{
	New: func() any { return new(ffi.RunFastOutput) },
}

var (
	hostCallbackOnce sync.Once
	hostCallbackPtr  uintptr
)

func init() {
	for i := minPooledInputCount; i <= maxPooledInputCount; i++ {
		rawInputPools[i] = make(chan []ffi.RawValue, 64)
	}
}

// Compile compiles Python code into a reusable Program.
func Compile(code string, opts ...CompileOption) (*Program, error) {
	config := compileConfig{scriptName: "main.py", autoStubs: true}
	for _, opt := range opts {
		opt(&config)
	}
	functions := make(map[string]*Function, len(config.functions))
	for _, function := range config.functions {
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
	return program, nil
}

// compileRunScratch bundles the per-call buffers needed by CompileAndRun.
// Pooling them keeps the fused fast path allocation-free for typical input
// counts. The embedded config keeps RunOption-applied state on the pooled
// allocation instead of forcing a fresh heap alloc per call.
type compileRunScratch struct {
	config   runConfig
	args     ffi.CompileRunFastRawArgs
	output   ffi.RunFastOutput
	nameRefs []ffi.Str
	raw      []ffi.RawValue
	arena    rawArena
}

var compileRunScratchPool = sync.Pool{
	New: func() any {
		return &compileRunScratch{
			nameRefs: make([]ffi.Str, 0, 8),
			raw:      make([]ffi.RawValue, 0, 8),
		}
	},
}

// CompileAndRun compiles code, runs it once with inputs, and frees the
// underlying Rust program — all in a single FFI hop. It is materially faster
// than Compile + Run + Close for one-shot evaluations because it pays the
// cgocall trampoline cost once instead of three times.
//
// The inputs argument is normalized through the same rules as RunAs.
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
	scratch := compileRunScratchPool.Get().(*compileRunScratch) //nolint:errcheck // pool only stores *compileRunScratch
	defer func() {
		clear(scratch.nameRefs)
		clear(scratch.raw)
		scratch.nameRefs = scratch.nameRefs[:0]
		scratch.raw = scratch.raw[:0]
		scratch.arena = rawArena{}
		scratch.args = ffi.CompileRunFastRawArgs{}
		scratch.output = ffi.RunFastOutput{}
		scratch.config = runConfig{}
		compileRunScratchPool.Put(scratch)
	}()
	for _, opt := range opts {
		opt(&scratch.config)
	}
	if deadline, ok := ctx.Deadline(); ok {
		scratch.config.limits = limitsWithContextDeadline(scratch.config.limits, deadline)
	}
	if scratch.config.needsDispatchLoop() {
		inputNames := make([]string, 0, len(values))
		for name := range values {
			inputNames = append(inputNames, name)
		}
		program, err := Compile(code, WithInputs(inputNames...))
		if err != nil {
			return Value{}, err
		}
		defer program.Close()
		return program.Run(ctx, values, opts...)
	}
	for name, value := range values {
		rawInput, err := rawValue(value, &scratch.arena)
		if err != nil {
			return Value{}, err
		}
		scratch.nameRefs = append(scratch.nameRefs, ffi.StringRef(name))
		scratch.raw = append(scratch.raw, rawInput)
	}
	var nameRefsPtr, rawPtr unsafe.Pointer
	if len(scratch.nameRefs) > 0 {
		nameRefsPtr = unsafe.Pointer(unsafe.SliceData(scratch.nameRefs))
		rawPtr = unsafe.Pointer(unsafe.SliceData(scratch.raw))
	}
	var ffiLimits *ffi.Limits
	if scratch.config.limits != nil {
		ffiLimits = scratch.config.limits.ffi()
	}
	scratch.args = ffi.CompileRunFastRawArgs{
		Code:            ffi.StringRef(code),
		ScriptName:      ffi.StringRef("main.py"),
		InputNames:      nameRefsPtr,
		InputCount:      uintptr(len(scratch.nameRefs)),
		InputValues:     rawPtr,
		InputValueCount: uintptr(len(scratch.raw)),
		Limits:          ffiLimits,
	}
	printed, err := ffi.ProgramCompileRunFastRaw(&scratch.args, &scratch.output)
	runtime.KeepAlive(values)
	writeErr := writePrinted(scratch.config.stdout, printed)
	if err != nil {
		ffi.RawValueFree(&scratch.output.Value)
		freeFastOutputBytes(&scratch.output)
		return Value{}, joinErrors(normalizeError(err), writeErr)
	}
	if writeErr != nil {
		ffi.RawValueFree(&scratch.output.Value)
		freeFastOutputBytes(&scratch.output)
		return Value{}, writeErr
	}
	return decodeFastOutput(&scratch.output)
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
	var snapshot []byte
	err := p.withHandle(func(handle uintptr) error {
		var dumpErr error
		snapshot, dumpErr = ffi.ProgramDump(handle)
		return dumpErr
	})
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
func (p *Program) Start(ctx context.Context, inputs any, opts ...RunOption) (Progress, error) {
	if p == nil {
		return nil, fmt.Errorf("monty: program is closed")
	}
	var config runConfig
	if len(opts) != 0 {
		config = p.runConfig(opts...)
	}
	return p.start(ctx, inputs, config)
}

func (p *Program) start(ctx context.Context, inputs any, config runConfig) (Progress, error) {
	var err error
	config, err = p.prepareRun(ctx, config)
	if err != nil {
		return nil, err
	}
	rawInputs, keepAlive, err := p.rawInputs(inputs)
	if err != nil {
		return nil, err
	}

	var ffiLimits *ffi.Limits
	if config.limits != nil {
		ffiLimits = config.limits.ffi()
	}
	var progressHandle uintptr
	var printed string
	err = p.withHandle(func(handle uintptr) error {
		var startErr error
		progressHandle, printed, startErr = ffi.ProgramStartRaw(handle, rawInputs, ffiLimits)
		return startErr
	})
	p.releaseRawInputs(rawInputs, keepAlive)
	runtime.KeepAlive(keepAlive)
	writeErr := writePrinted(config.stdout, printed)
	if err != nil {
		return nil, joinErrors(normalizeError(err), writeErr)
	}
	if writeErr != nil {
		ffi.ProgressFree(progressHandle)
		return nil, writeErr
	}
	return progressFromHandle(progressHandle, config.stdout)
}

func (p *Program) runDirectFast(inputs any, config runConfig) (Value, error) {
	rawInputs, keepAlive, err := p.rawInputs(inputs)
	if err != nil {
		return Value{}, err
	}
	var ffiLimits *ffi.Limits
	if config.limits != nil {
		ffiLimits = config.limits.ffi()
	}
	output := runFastOutputPool.Get().(*ffi.RunFastOutput) //nolint:errcheck // pool only stores *ffi.RunFastOutput
	defer runFastOutputPool.Put(output)
	var printed string
	p.mu.RLock()
	if p.handle == 0 {
		p.mu.RUnlock()
		p.releaseRawInputs(rawInputs, keepAlive)
		runtime.KeepAlive(keepAlive)
		return Value{}, fmt.Errorf("monty: program is closed")
	}
	printed, err = ffi.ProgramRunFastRaw(p.handle, rawInputs, ffiLimits, output)
	p.mu.RUnlock()
	p.releaseRawInputs(rawInputs, keepAlive)
	runtime.KeepAlive(keepAlive)
	writeErr := writePrinted(config.stdout, printed)
	if err != nil {
		ffi.RawValueFree(&output.Value)
		freeFastOutputBytes(output)
		return Value{}, joinErrors(normalizeError(err), writeErr)
	}
	if writeErr != nil {
		ffi.RawValueFree(&output.Value)
		freeFastOutputBytes(output)
		return Value{}, writeErr
	}
	return decodeFastOutput(output)
}

func (p *Program) runDirectRaw(inputs any, config runConfig) (ffi.RawValue, error) {
	rawInputs, keepAlive, err := p.rawInputs(inputs)
	if err != nil {
		return ffi.RawValue{}, err
	}
	var ffiLimits *ffi.Limits
	if config.limits != nil {
		ffiLimits = config.limits.ffi()
	}
	var rawResult ffi.RawValue
	var printed string
	p.mu.RLock()
	if p.handle == 0 {
		p.mu.RUnlock()
		p.releaseRawInputs(rawInputs, keepAlive)
		runtime.KeepAlive(keepAlive)
		return ffi.RawValue{}, fmt.Errorf("monty: program is closed")
	}
	rawResult, printed, err = ffi.ProgramRunRaw(p.handle, rawInputs, ffiLimits)
	p.mu.RUnlock()
	p.releaseRawInputs(rawInputs, keepAlive)
	runtime.KeepAlive(keepAlive)
	writeErr := writePrinted(config.stdout, printed)
	if err != nil {
		return ffi.RawValue{}, joinErrors(normalizeError(err), writeErr)
	}
	if writeErr != nil {
		ffi.RawValueFree(&rawResult)
		return ffi.RawValue{}, writeErr
	}
	return rawResult, nil
}

func (p *Program) runDirectRawInt(inputs any, config runConfig) (int64, Kind, error) {
	rawInputs, keepAlive, err := p.rawInputs(inputs)
	if err != nil {
		return 0, InvalidKind, err
	}
	var ffiLimits *ffi.Limits
	if config.limits != nil {
		ffiLimits = config.limits.ffi()
	}
	var value int64
	var kind uint32
	var printed string
	p.mu.RLock()
	if p.handle == 0 {
		p.mu.RUnlock()
		p.releaseRawInputs(rawInputs, keepAlive)
		runtime.KeepAlive(keepAlive)
		return 0, InvalidKind, fmt.Errorf("monty: program is closed")
	}
	value, kind, printed, err = ffi.ProgramRunRawInt(p.handle, rawInputs, ffiLimits)
	p.mu.RUnlock()
	p.releaseRawInputs(rawInputs, keepAlive)
	runtime.KeepAlive(keepAlive)
	writeErr := writePrinted(config.stdout, printed)
	if err != nil {
		return 0, InvalidKind, joinErrors(normalizeError(err), writeErr)
	}
	if writeErr != nil {
		return 0, InvalidKind, writeErr
	}
	return value, Kind(kind), nil
}

func (p *Program) runDirectRawText(inputs any, config runConfig) (string, Kind, error) {
	rawInputs, keepAlive, err := p.rawInputs(inputs)
	if err != nil {
		return "", InvalidKind, err
	}
	var ffiLimits *ffi.Limits
	if config.limits != nil {
		ffiLimits = config.limits.ffi()
	}
	var value string
	var kind uint32
	var printed string
	p.mu.RLock()
	if p.handle == 0 {
		p.mu.RUnlock()
		p.releaseRawInputs(rawInputs, keepAlive)
		runtime.KeepAlive(keepAlive)
		return "", InvalidKind, fmt.Errorf("monty: program is closed")
	}
	value, kind, printed, err = ffi.ProgramRunRawText(p.handle, rawInputs, ffiLimits)
	p.mu.RUnlock()
	p.releaseRawInputs(rawInputs, keepAlive)
	runtime.KeepAlive(keepAlive)
	writeErr := writePrinted(config.stdout, printed)
	if err != nil {
		return "", InvalidKind, joinErrors(normalizeError(err), writeErr)
	}
	if writeErr != nil {
		return "", InvalidKind, writeErr
	}
	return value, Kind(kind), nil
}

// Run executes the Program until it completes and returns the final Python value.
//
// Registered Go functions are resolved automatically. Runs without registered
// functions use a direct FFI path for lower overhead.
func (p *Program) Run(ctx context.Context, inputs any, opts ...RunOption) (Value, error) {
	if p == nil {
		return Value{}, fmt.Errorf("monty: program is closed")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var config runConfig
	if len(opts) == 0 {
		config.functions = p.functions
		config.functionNames = p.funcNames
	} else {
		config = p.runConfig(opts...)
	}
	if !config.needsDispatchLoop() {
		preparedConfig, err := p.prepareRun(ctx, config)
		if err != nil {
			return Value{}, err
		}
		return p.runDirectFast(inputs, preparedConfig)
	}
	if config.canUseFunctionCallbackFast() {
		return p.runFunctionCallback(ctx, inputs, config)
	}
	if config.canUseFunctionDispatchFast() {
		return p.runFunctionDispatchFast(ctx, inputs, config)
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
	var err error
	if ctx == nil {
		ctx = context.Background()
	}
	config, err = p.prepareRun(ctx, config)
	if err != nil {
		return ffi.RawValue{}, err
	}
	rawInputs, keepAlive, err := p.rawInputs(inputs)
	if err != nil {
		return ffi.RawValue{}, err
	}
	var ffiLimits *ffi.Limits
	if config.limits != nil {
		ffiLimits = config.limits.ffi()
	}
	names := config.functionNames
	if names == nil && len(config.functions) != 0 {
		names = hostFunctionNameRefs(config.functions)
	}
	hostCallbackOnce.Do(func() {
		hostCallbackPtr = ffi.NewCallback(hostFunctionCallback)
	})
	callback := hostCallbackPtr
	state := &hostCallbackState{ctx: ctx, functions: config.functions}
	p.mu.RLock()
	if p.handle == 0 {
		p.mu.RUnlock()
		p.releaseRawInputs(rawInputs, keepAlive)
		runtime.KeepAlive(keepAlive)
		return ffi.RawValue{}, fmt.Errorf("monty: program is closed")
	}
	rawResult, printed, err := ffi.ProgramRunHostRaw(
		p.handle,
		rawInputs,
		ffiLimits,
		names,
		callback,
		uintptr(unsafe.Pointer(state)),
	)
	p.mu.RUnlock()
	p.releaseRawInputs(rawInputs, keepAlive)
	runtime.KeepAlive(keepAlive)
	runtime.KeepAlive(names)
	runtime.KeepAlive(state)
	writeErr := writePrinted(config.stdout, printed)
	if err != nil {
		return ffi.RawValue{}, joinErrors(normalizeError(err), writeErr)
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

func hostFunctionCallback(userData unsafe.Pointer, namePtr unsafe.Pointer, nameLen uintptr, argsPtr unsafe.Pointer, kwargsPtr unsafe.Pointer, outPtr unsafe.Pointer) int32 {
	if outPtr == nil {
		return ffi.HostCallbackException
	}
	out := (*ffi.HostFunctionOutput)(outPtr)
	*out = ffi.HostFunctionOutput{}
	if userData == nil {
		return ffi.HostCallbackException
	}
	state := (*hostCallbackState)(userData)
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
	s.setException(excType, message)
	out.ExcType = ffi.StringRef(s.excType)
	out.Message = ffi.StringRef(s.message)
	return ffi.HostCallbackException
}

func (s *hostCallbackState) setException(excType, message string) {
	if excType == "" {
		excType = "RuntimeError"
	}
	s.excType = excType
	s.message = message
}

func (p *Program) runFunctionDispatchFast(ctx context.Context, inputs any, config runConfig) (Value, error) {
	var err error
	config, err = p.prepareRun(ctx, config)
	if err != nil {
		return Value{}, err
	}
	rawInputs, keepAlive, err := p.rawInputs(inputs)
	if err != nil {
		return Value{}, err
	}
	var ffiLimits *ffi.Limits
	if config.limits != nil {
		ffiLimits = config.limits.ffi()
	}
	p.mu.RLock()
	if p.handle == 0 {
		p.mu.RUnlock()
		p.releaseRawInputs(rawInputs, keepAlive)
		runtime.KeepAlive(keepAlive)
		return Value{}, fmt.Errorf("monty: program is closed")
	}
	progressHandle, snapshot, printed, err := ffi.ProgramStartRawSnapshot(p.handle, rawInputs, ffiLimits)
	p.mu.RUnlock()
	p.releaseRawInputs(rawInputs, keepAlive)
	runtime.KeepAlive(keepAlive)
	progressHandle, snapshot, err = finishProgressStep(config.stdout, progressHandle, snapshot, printed, err)
	if err != nil {
		return Value{}, err
	}
	for {
		if err := ctx.Err(); err != nil {
			if progressHandle != 0 {
				ffi.ProgressFree(progressHandle)
			}
			freeProgressSnapshot(snapshot)
			return Value{}, err
		}
		switch snapshot.Kind {
		case ffi.ProgressComplete:
			value, err := decodeRawValue(snapshot.Value)
			return value, normalizeError(err)
		case ffi.ProgressNameLookup:
			function := functionByNameRef(snapshot.Name, config.functions)
			freeSnapshotName(&snapshot)
			var next uintptr
			var nextSnapshot ffi.ProgressSnapshot
			var resumePrinted string
			if function == nil {
				next, nextSnapshot, resumePrinted, err = ffi.ProgressResumeNameUndefinedSnapshot(progressHandle)
			} else {
				arena := &rawArena{}
				var rawFunction ffi.RawValue
				rawFunction, err = rawValue(ExternalFunction(function.Name()), arena)
				if err != nil {
					ffi.ProgressFree(progressHandle)
					return Value{}, err
				}
				next, nextSnapshot, resumePrinted, err = ffi.ProgressResumeNameValueRawSnapshot(progressHandle, &rawFunction)
				freeOwnedRawValue(&rawFunction)
				runtime.KeepAlive(arena)
			}
			progressHandle, snapshot, err = finishProgressStep(config.stdout, next, nextSnapshot, resumePrinted, err)
			if err != nil {
				return Value{}, err
			}
		case ffi.ProgressFunctionCall:
			function := functionByNameRef(snapshot.Name, config.functions)
			if function == nil {
				name := ffi.TakeString(snapshot.Name)
				snapshot.Name = ffi.Bytes{}
				freeProgressSnapshot(snapshot)
				ffi.ProgressFree(progressHandle)
				return Value{}, fmt.Errorf("monty: external function %q called but no Go function was registered", name)
			}
			freeSnapshotName(&snapshot)
			var raw ffi.RawValue
			var arena *rawArena
			raw, arena, err = invokeHostFunctionRaw(ctx, function, &snapshot)
			var next uintptr
			var nextSnapshot ffi.ProgressSnapshot
			var resumePrinted string
			if err != nil {
				excType, message := exceptionFromError(err)
				next, resumePrinted, err = ffi.ProgressResumeException(progressHandle, excType, message)
				if err == nil {
					nextSnapshot, err = ffi.ProgressSnapshotGet(next)
				}
			} else {
				next, nextSnapshot, resumePrinted, err = ffi.ProgressResumeReturnRawSnapshot(progressHandle, &raw)
				freeOwnedRawValue(&raw)
				runtime.KeepAlive(arena)
			}
			progressHandle, snapshot, err = finishProgressStep(config.stdout, next, nextSnapshot, resumePrinted, err)
			if err != nil {
				return Value{}, err
			}
		default:
			freeProgressSnapshot(snapshot)
			progress, err := progressFromHandle(progressHandle, config.stdout)
			if err != nil {
				return Value{}, err
			}
			return p.runProgressLoop(ctx, progress, config)
		}
	}
}

// finishProgressStep flushes the print buffer after a snapshot-style FFI step
// (start or resume) and either advances the loop to (next, nextSnapshot) or
// returns the joined error after freeing the partially-built next state.
func finishProgressStep(
	stdout io.Writer,
	next uintptr,
	nextSnapshot ffi.ProgressSnapshot,
	printed string,
	callErr error,
) (uintptr, ffi.ProgressSnapshot, error) {
	writeErr := writePrinted(stdout, printed)
	if callErr != nil || writeErr != nil {
		if next != 0 {
			ffi.ProgressFree(next)
		}
		freeProgressSnapshot(nextSnapshot)
		return 0, ffi.ProgressSnapshot{}, joinErrors(normalizeError(callErr), writeErr)
	}
	return next, nextSnapshot, nil
}

// invokeHostFunctionRaw runs function with the snapshot's positional and
// keyword args and returns the marshaled raw result. The fast path passes the
// raw args through directly; the slow path decodes them into typed Values.
// Either way, snapshot.Args and snapshot.Kwargs are consumed.
func invokeHostFunctionRaw(ctx context.Context, function *Function, snapshot *ffi.ProgressSnapshot) (ffi.RawValue, *rawArena, error) {
	if function.fastRawCall != nil {
		raw, err := function.fastRawCall(ctx, snapshot.Args, snapshot.Kwargs)
		ffi.RawValueFree(&snapshot.Args)
		ffi.RawValueFree(&snapshot.Kwargs)
		return raw, nil, err
	}
	args, err := valuesFromRawList(snapshot.Args)
	if err != nil {
		ffi.RawValueFree(&snapshot.Kwargs)
		return ffi.RawValue{}, nil, err
	}
	kwargs, err := pairsFromRawDict(snapshot.Kwargs)
	if err != nil {
		return ffi.RawValue{}, nil, err
	}
	result, err := function.call(ctx, args, kwargs)
	if err != nil {
		return ffi.RawValue{}, nil, err
	}
	arena := &rawArena{}
	raw, err := rawValue(result, arena)
	runtime.KeepAlive(result)
	return raw, arena, err
}

func functionByNameRef(name ffi.Bytes, functions map[string]*Function) *Function {
	if name.Len == 0 {
		return functions[""]
	}
	if name.Ptr == nil {
		return nil
	}
	text := unsafe.String((*byte)(name.Ptr), int(name.Len))
	return functions[text]
}

func freeSnapshotName(snapshot *ffi.ProgressSnapshot) {
	ffi.MaybeBytesFree(snapshot.Name)
	snapshot.Name = ffi.Bytes{}
}

func freeProgressSnapshot(snapshot ffi.ProgressSnapshot) {
	ffi.MaybeBytesFree(snapshot.Name)
	ffi.RawValueFree(&snapshot.Args)
	ffi.RawValueFree(&snapshot.Kwargs)
	ffi.RawValueFree(&snapshot.Value)
}

// RunAs executes program and converts the final Python value into T.
//
// Primitive Go types, structs, maps, slices, and Value are supported through
// the same conversion rules as As.
func RunAs[T any](ctx context.Context, program *Program, inputs any, opts ...RunOption) (T, error) {
	var zero T
	if program == nil {
		return zero, fmt.Errorf("monty: program is closed")
	}
	var config runConfig
	if len(opts) == 0 {
		config.functions = program.functions
		config.functionNames = program.funcNames
	} else {
		config = program.runConfig(opts...)
	}
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
	value, err := program.Run(ctx, inputs, opts...)
	if err != nil {
		return zero, err
	}
	result, err := As[T](value)
	if err != nil {
		return zero, err
	}
	return result, nil
}

func runAsDirect[T any](ctx context.Context, program *Program, inputs any, config runConfig) (T, error) {
	var zero T
	config, err := program.prepareRun(ctx, config)
	if err != nil {
		return zero, err
	}
	// T is narrowed by each case below; the inner assertions to T cannot fail.
	switch any(zero).(type) {
	case int:
		value, kind, err := program.runDirectRawInt(inputs, config)
		if err != nil {
			return zero, err
		}
		if kind != IntKind {
			return zero, fmt.Errorf("monty: cannot convert %s to int", kind)
		}
		return any(int(value)).(T), nil //nolint:errcheck // T is int here
	case int64:
		value, kind, err := program.runDirectRawInt(inputs, config)
		if err != nil {
			return zero, err
		}
		if kind != IntKind {
			return zero, fmt.Errorf("monty: cannot convert %s to int64", kind)
		}
		return any(value).(T), nil //nolint:errcheck // T is int64 here
	case string:
		value, kind, err := program.runDirectRawText(inputs, config)
		if err != nil {
			return zero, err
		}
		if !isStringLikeKind(kind) {
			return zero, fmt.Errorf("monty: cannot convert %s to string", kind)
		}
		return any(value).(T), nil //nolint:errcheck // T is string here
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

func (p *Program) withHandle(call func(uintptr) error) error {
	if p == nil {
		return fmt.Errorf("monty: program is closed")
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.handle == 0 {
		return fmt.Errorf("monty: program is closed")
	}
	return call(p.handle)
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

func (p *Program) rawInputs(inputs any) ([]ffi.RawValue, rawInputKeepAlive, error) {
	inputValues, err := normalizeInputs(inputs)
	if err != nil {
		return nil, rawInputKeepAlive{}, err
	}
	var arena rawArena
	rawInputs := p.getRawInputs()
	for i, name := range p.inputs {
		value, ok := inputValues[name]
		if !ok {
			p.putRawInputs(rawInputs, arena.ownsHandles)
			return nil, rawInputKeepAlive{}, fmt.Errorf("monty: missing input %q", name)
		}
		rawInput, err := rawValue(value, &arena)
		if err != nil {
			p.putRawInputs(rawInputs, arena.ownsHandles)
			return nil, rawInputKeepAlive{}, err
		}
		rawInputs[i] = rawInput
	}
	return rawInputs, rawInputKeepAlive{inputValues: inputValues, arena: arena}, nil
}

func (p *Program) getRawInputs() []ffi.RawValue {
	if len(p.inputs) == 0 {
		return nil
	}
	if pool := rawInputPool(len(p.inputs)); pool != nil {
		select {
		case raw := <-pool:
			return raw[:len(p.inputs)]
		default:
		}
	}
	return make([]ffi.RawValue, len(p.inputs))
}

func (p *Program) releaseRawInputs(raw []ffi.RawValue, keepAlive rawInputKeepAlive) {
	p.putRawInputs(raw, keepAlive.arena.ownsHandles)
}

func (p *Program) putRawInputs(raw []ffi.RawValue, freeOwned bool) {
	if len(raw) == 0 {
		return
	}
	if freeOwned {
		freeOwnedRawValues(raw)
	}
	clear(raw)
	if pool := rawInputPool(len(raw)); pool != nil {
		select {
		case pool <- raw:
		default:
		}
	}
}

func rawInputPool(count int) chan []ffi.RawValue {
	if count < minPooledInputCount || count > maxPooledInputCount {
		return nil
	}
	return rawInputPools[count]
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
