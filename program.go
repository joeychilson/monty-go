package monty

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"unsafe"

	"github.com/joeychilson/monty/internal/ffi"
)

// Program is a compiled Monty Python program.
//
// A Program is immutable after Compile and safe for concurrent runs. Call
// Close when it is no longer needed to release the Rust-side handle.
type Program struct {
	mu          sync.RWMutex
	handle      uintptr
	code        string
	scriptName  string
	inputs      []string
	functions   map[string]*Function
	funcNames   []ffi.Str
	dataclasses []*DataclassType
	cleanup     runtime.Cleanup
}

var runFastOutputPool = sync.Pool{
	New: func() any { return new(ffi.RunFastOutput) },
}

// Compile compiles Python code into a reusable Program.
//
// Compilation failures return *SyntaxError; enabled type checking returns
// *TypeCheckError when the code has type errors.
func Compile(code string, opts ...CompileOption) (*Program, error) {
	config := compileConfig{scriptName: "main.py"}
	for _, opt := range opts {
		opt.applyCompile(&config)
	}
	functions := make(map[string]*Function, len(config.functions))
	for _, function := range config.functions {
		if _, exists := functions[function.Name()]; exists {
			return nil, fmt.Errorf("monty: duplicate function %q", function.Name())
		}
		functions[function.Name()] = function
	}
	if config.typeCheck {
		if err := typeCheck(code, config.scriptName, config.stubs, functions, config.dataclasses); err != nil {
			return nil, err
		}
	}
	handle, err := ffi.ProgramCompile(code, config.scriptName, config.inputs)
	if err != nil {
		return nil, compileError(err)
	}
	program := &Program{
		handle:      handle,
		code:        code,
		scriptName:  config.scriptName,
		inputs:      slices.Clone(config.inputs),
		functions:   functions,
		funcNames:   hostFunctionNameRefs(functions),
		dataclasses: config.dataclasses,
	}
	program.cleanup = runtime.AddCleanup(program, ffi.ProgramFree, handle)
	return program, nil
}

// typeCheck runs the static checker over code plus generated and explicit
// stubs.
func typeCheck(code, scriptName, extraStubs string, functions map[string]*Function, dataclasses []*DataclassType) error {
	stubs := joinStubs(extraStubs, functions, dataclasses)
	diags, err := ffi.TypeCheck(code, scriptName, stubs, "type_stubs.pyi")
	if err != nil {
		return execError(err)
	}
	if diags != 0 {
		return newTypeCheckError(diags)
	}
	return nil
}

func joinStubs(prefix string, functions map[string]*Function, dataclasses []*DataclassType) string {
	parts := make([]string, 0, len(functions)+len(dataclasses)+1)
	if strings.TrimSpace(prefix) != "" {
		parts = append(parts, prefix)
	}
	for _, dataclass := range dataclasses {
		parts = append(parts, dataclass.Stub())
	}
	for _, function := range functions {
		parts = append(parts, function.PythonStub())
	}
	return strings.Join(parts, "\n\n")
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

// Close releases the Rust-side program handle. Close is idempotent.
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

// Dump serializes the compiled Program so it can be loaded in another process.
func (p *Program) Dump() ([]byte, error) {
	if p == nil {
		return nil, ErrClosed
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.handle == 0 {
		return nil, ErrClosed
	}
	snapshot, err := ffi.ProgramDump(p.handle)
	return snapshot, normalizeError(err)
}

// Code returns the source code used to compile the Program.
func (p *Program) Code() string { return p.code }

// ScriptName returns the filename Monty uses in tracebacks and diagnostics.
func (p *Program) ScriptName() string { return p.scriptName }

// InputNames returns the ordered input names declared at compile time.
func (p *Program) InputNames() []string { return slices.Clone(p.inputs) }

// TypeCheck runs the static type checker over the program's code together
// with stubs generated from its registered functions and dataclasses, plus
// any extra stub text. It returns nil or a *TypeCheckError.
func (p *Program) TypeCheck(extraStubs ...string) error {
	if p == nil {
		return ErrClosed
	}
	return typeCheck(p.code, p.scriptName, strings.Join(extraStubs, "\n\n"), p.functions, p.dataclasses)
}

// --------------------------------------------------------------------------
// Run paths
// --------------------------------------------------------------------------

// Run executes the Program to completion and returns the final Python value.
//
// Registered functions, mounts, WithFS filesystems, and the OS handler are
// dispatched automatically; WithAsync functions run concurrently where Python
// gathers them. Runs with no dispatch needs use a direct single-hop FFI path.
//
// Cancellation: a ctx deadline is enforced as a hard time limit inside the
// interpreter. Plain cancellation (context.WithCancel) is forwarded to the
// interpreter at Python statement boundaries and additionally observed
// between dispatch steps.
func (p *Program) Run(ctx context.Context, inputs any, opts ...RunOption) (Value, error) {
	if p == nil {
		return Value{}, ErrClosed
	}
	return p.run(ctx, inputs, p.runConfig(opts...))
}

// RunAs executes program and converts the final Python value into T using the
// same rules as As.
func RunAs[T any](ctx context.Context, program *Program, inputs any, opts ...RunOption) (T, error) {
	var zero T
	if program == nil {
		return zero, ErrClosed
	}
	config := program.runConfig(opts...)
	if err := config.validateForProgram(); err != nil {
		return zero, err
	}
	if !config.needsHostPath() || config.hostPathEligible() {
		output := runFastOutputPool.Get().(*ffi.RunFastOutput) //nolint:errcheck // pool only stores *ffi.RunFastOutput
		defer runFastOutputPool.Put(output)
		if err := program.runFastInto(ctx, inputs, config, output); err != nil {
			return zero, err
		}
		return fastOutputAs[T](output)
	}
	value, err := program.run(ctx, inputs, config)
	if err != nil {
		return zero, err
	}
	return As[T](value)
}

// RunJSON executes the Program and returns the final value in Monty's natural
// JSON form (tagged objects for Python-only types).
func (p *Program) RunJSON(ctx context.Context, inputs any, opts ...RunOption) ([]byte, error) {
	if p == nil {
		return nil, ErrClosed
	}
	config := p.runConfig(opts...)
	if err := config.validateForProgram(); err != nil {
		return nil, err
	}
	if !config.needsHostPath() {
		return p.runJSONDirect(ctx, inputs, config)
	}
	value, err := p.run(ctx, inputs, config)
	if err != nil {
		return nil, err
	}
	return value.MarshalJSON()
}

// Start begins executing the Program and returns its Run.
//
// Use Start to resolve external calls manually, persist snapshots, or drive
// async futures; for run-to-value execution use Run. Configured dispatch
// (functions, mounts, WithFS, OS handler) is applied automatically between
// pauses, so only unhandled interrupts surface. Start itself fails only for
// binding-level problems; a Python-level failure is reported by Run.Result.
func (p *Program) Start(ctx context.Context, inputs any, opts ...RunOption) (*Run, error) {
	if p == nil {
		return nil, ErrClosed
	}
	return p.start(ctx, inputs, p.runConfig(opts...), nil)
}

func (p *Program) start(ctx context.Context, inputs any, config runConfig, repl *REPL) (*Run, error) {
	if err := config.validateForProgram(); err != nil {
		return nil, err
	}
	ctx, config, err := prepareRun(ctx, config)
	if err != nil {
		return nil, err
	}
	if err := config.openMounts(); err != nil {
		return nil, err
	}
	rawInputs, keepAlive, err := p.rawInputs(inputs)
	if err != nil {
		return nil, err
	}
	run := &Run{state: statePaused, config: config, repl: repl}
	if ctx.Done() != nil {
		if token, tokenErr := ffi.CancelTokenNew(); tokenErr == nil {
			run.cancelToken = token
		}
	}
	limits := config.ffiLimits(run.cancelToken)
	var step ffi.StepResult
	stop := run.watchCancelLocked(ctx)
	callErr := p.callLocked(rawInputs, keepAlive, func(handle uintptr) error {
		var ffiErr error
		step, ffiErr = ffi.ProgramStartRawSnapshot(handle, rawInputs, limits)
		return ffiErr
	})
	stop()
	runtime.KeepAlive(limits)
	run.mu.Lock()
	run.advanceLocked(ctx, step, callErr)
	run.mu.Unlock()
	return run, nil
}

// run executes with full dispatch, choosing the cheapest capable path.
func (p *Program) run(ctx context.Context, inputs any, config runConfig) (Value, error) {
	if err := config.validateForProgram(); err != nil {
		return Value{}, err
	}
	if !config.needsHostPath() || config.hostPathEligible() {
		output := runFastOutputPool.Get().(*ffi.RunFastOutput) //nolint:errcheck // pool only stores *ffi.RunFastOutput
		defer runFastOutputPool.Put(output)
		if err := p.runFastInto(ctx, inputs, config, output); err != nil {
			return Value{}, err
		}
		return decodeFastOutput(output)
	}
	run, err := p.start(ctx, inputs, config, nil)
	if err != nil {
		return Value{}, err
	}
	return drainRun(ctx, run)
}

// drainRun drives a Run to completion for run-to-value execution: leftover
// interrupts that auto-dispatch could not answer are resolved with safe
// defaults or reported as errors.
func drainRun(ctx context.Context, run *Run) (Value, error) {
	defer run.Close()
	for run.Paused() {
		if err := ctx.Err(); err != nil {
			return Value{}, err
		}
		switch req := run.Pending().(type) {
		case *Call:
			if req.OS {
				_ = req.NotHandled(ctx) //nolint:errcheck // failure is reported by Result below
				continue
			}
			return Value{}, fmt.Errorf("monty: external function %q called but no Go function was registered", req.Name)
		case *NameLookup:
			_ = req.Undefined(ctx) //nolint:errcheck // failure is reported by Result below
		case *Gather:
			return Value{}, fmt.Errorf("monty: %d deferred calls cannot be resolved: their functions are not registered", len(req.CallIDs()))
		default:
			return Value{}, fmt.Errorf("monty: unsupported interrupt %T in Run", req)
		}
	}
	return run.Result()
}

// runFastInto executes through one of the single-hop paths (direct or
// host-callback) into a pooled fast output owned by the caller.
func (p *Program) runFastInto(ctx context.Context, inputs any, config runConfig, output *ffi.RunFastOutput) error {
	ctx, config, err := prepareRun(ctx, config)
	if err != nil {
		return err
	}
	if err := config.openMounts(); err != nil {
		return err
	}
	rawInputs, keepAlive, err := p.rawInputs(inputs)
	if err != nil {
		return err
	}
	var token uintptr
	if ctx.Done() != nil {
		if token, err = ffi.CancelTokenNew(); err == nil {
			defer ffi.CancelTokenFree(token)
		} else {
			token = 0
		}
	}
	limits := config.ffiLimits(token)

	if !config.needsHostPath() {
		stop := maybeWatchCancel(ctx, token)
		callErr := p.callLocked(rawInputs, keepAlive, func(handle uintptr) error {
			return ffi.ProgramRunFastRaw(handle, rawInputs, limits, output)
		})
		stop()
		runtime.KeepAlive(limits)
		return p.finishFastOutput(ctx, config, output, callErr, nil)
	}

	scratch := hostScratchPool.Get().(*hostRunScratch) //nolint:errcheck // pool only stores *hostRunScratch
	defer hostScratchPool.Put(scratch)
	state := &scratch.state
	state.reset(ctx, &config)
	mounts := config.mountFFIHandles()
	names := config.functionNameRefs(p)
	scratch.args = ffi.RunHostArgs{
		Inputs:        slicePtr(rawInputs),
		InputCount:    uintptr(len(rawInputs)),
		Limits:        limits,
		HostNames:     slicePtr(names),
		HostNameCount: uintptr(len(names)),
		Mounts:        slicePtr(mounts),
		MountCount:    uintptr(len(mounts)),
		Callback:      hostCallbackPtr,
		CallbackData:  uintptr(unsafe.Pointer(state)),
	}
	if config.stdout != nil || config.stderr != nil {
		scratch.args.Print = printCallbackPtr
		scratch.args.PrintData = uintptr(unsafe.Pointer(state))
	}
	stop := maybeWatchCancel(ctx, token)
	callErr := p.callLocked(rawInputs, keepAlive, func(handle uintptr) error {
		return ffi.ProgramRunHostRaw(handle, &scratch.args, output)
	})
	stop()
	runtime.KeepAlive(limits)
	runtime.KeepAlive(names)
	runtime.KeepAlive(mounts)
	runtime.KeepAlive(scratch)
	err = p.finishFastOutput(ctx, config, output, callErr, state)
	state.clear()
	return err
}

// finishFastOutput flushes buffered print output and folds call, write, and
// host-callback errors into the final result.
func (p *Program) finishFastOutput(ctx context.Context, config runConfig, output *ffi.RunFastOutput, callErr error, state *hostCallbackState) error {
	printed := ffi.TakePrinted(output.Print, output.PrintFlags)
	output.Print = ffi.Bytes{}
	writeErr := writePrint(&config, printed)
	if state != nil && writeErr == nil {
		writeErr = state.writeErr
	}
	if callErr != nil {
		ffi.RawValueFree(&output.Value)
		freeFastOutputBytes(output)
		return errors.Join(wrapCtxErrorBound(ctx, execError(callErr), config.deadlineBound), writeErr)
	}
	if writeErr != nil {
		ffi.RawValueFree(&output.Value)
		freeFastOutputBytes(output)
		return writeErr
	}
	return nil
}

// wrapCtxErrorBound ties a limit/cancellation failure back to the context.
// deadlineBound covers the race where the interpreter's deadline-derived
// timer fires a moment before the context's own clock reports expiry.
func wrapCtxErrorBound(ctx context.Context, err error, deadlineBound bool) error {
	execErr, ok := errors.AsType[*ExecError](err)
	if !ok {
		return err
	}
	switch execErr.Type {
	case "TimeoutError", "KeyboardInterrupt":
	default:
		return err
	}
	var ctxErr error
	if ctx != nil {
		ctxErr = context.Cause(ctx)
	}
	if ctxErr == nil {
		if !deadlineBound || execErr.Type != "TimeoutError" {
			return err
		}
		ctxErr = context.DeadlineExceeded
	}
	return &ctxExecError{exec: execErr, ctxErr: ctxErr}
}

func maybeWatchCancel(ctx context.Context, token uintptr) func() {
	if token == 0 || ctx.Done() == nil {
		return func() {}
	}
	return watchCancel(ctx, token)
}

// freeFastOutputBytes releases the Rust-owned flat buffer when one was
// emitted. Outputs whose bytes were copied into the inline scratch require no
// FFI call; the buffer lives entirely on the Go side.
func freeFastOutputBytes(out *ffi.RunFastOutput) {
	if out.BytesInScratch == 0 {
		ffi.MaybeBytesFree(out.Bytes)
	}
	out.Bytes = ffi.Bytes{}
}

func decodeFastOutput(out *ffi.RunFastOutput) (Value, error) {
	if out.Format != ffi.FastFormatFlat {
		value, err := decodeRawValue(out.Value)
		// decodeRawValue consumed any owned handle; drop the stale pointer so
		// a pooled output can never present it for a second free.
		out.Value = ffi.RawValue{}
		return value, err
	}
	data := ffi.UnsafeBytes(out.Bytes)
	if len(data) > flatStringCopyThreshold {
		// At this size decodeFlatValue copies every string out of the buffer,
		// so the buffer (pooled scratch or Rust-owned heap) only has to live
		// until the decode returns — no up-front copy of the whole payload.
		value, err := decodeFlatValue(data)
		freeFastOutputBytes(out)
		return value, err
	}
	var owned []byte
	if len(data) != 0 {
		// Small result: decoded strings borrow from this buffer, so it must
		// outlive the returned Value tree instead of pointing at pooled
		// scratch memory.
		owned = make([]byte, len(data))
		copy(owned, data)
	}
	freeFastOutputBytes(out)
	return decodeFlatValue(owned)
}

func fastOutputAs[T any](out *ffi.RunFastOutput) (T, error) {
	var zero T
	if out.Format != ffi.FastFormatFlat {
		result, err := rawAs[T](out.Value)
		// rawAs consumed any owned handle or buffer; drop the stale pointer
		// so a pooled output can never present it for a second free.
		out.Value = ffi.RawValue{}
		return result, err
	}
	value, err := decodeFastOutput(out)
	if err != nil {
		return zero, err
	}
	return As[T](value)
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

func (p *Program) runJSONDirect(ctx context.Context, inputs any, config runConfig) ([]byte, error) {
	ctx, config, err := prepareRun(ctx, config)
	if err != nil {
		return nil, err
	}
	rawInputs, keepAlive, err := p.rawInputs(inputs)
	if err != nil {
		return nil, err
	}
	var token uintptr
	if ctx.Done() != nil {
		if token, err = ffi.CancelTokenNew(); err == nil {
			defer ffi.CancelTokenFree(token)
		} else {
			token = 0
		}
	}
	limits := config.ffiLimits(token)
	var jsonBytes []byte
	var printed ffi.Printed
	stop := maybeWatchCancel(ctx, token)
	callErr := p.callLocked(rawInputs, keepAlive, func(handle uintptr) error {
		var ffiErr error
		jsonBytes, printed, ffiErr = ffi.ProgramRunJSONRaw(handle, rawInputs, limits)
		return ffiErr
	})
	stop()
	runtime.KeepAlive(limits)
	writeErr := writePrint(&config, printed)
	if callErr != nil {
		return nil, errors.Join(wrapCtxErrorBound(ctx, execError(callErr), config.deadlineBound), writeErr)
	}
	if writeErr != nil {
		return nil, writeErr
	}
	return jsonBytes, nil
}

// --------------------------------------------------------------------------
// One-shot evaluation
// --------------------------------------------------------------------------

// evalScriptName is the traceback filename Eval uses on both its single-hop
// path and its compile+run fallback, kept in one place so the two cannot
// drift.
const evalScriptName = "main.py"

// Eval compiles code, runs it once, and frees the underlying program — in a
// single FFI hop when no dispatch options are present. It is materially
// faster than Compile + Run + Close for one-shot evaluations.
func Eval(ctx context.Context, code string, inputs any, opts ...RunOption) (Value, error) {
	var config runConfig
	for _, opt := range opts {
		opt.applyRun(&config)
	}
	if err := config.validateForProgram(); err != nil {
		return Value{}, err
	}
	values, err := inputsToValues(inputs)
	if err != nil {
		return Value{}, err
	}
	if config.needsHostPath() {
		inputNames := make([]string, 0, len(values))
		for name := range values {
			inputNames = append(inputNames, name)
		}
		program, err := Compile(code, WithScriptName(evalScriptName), WithInputs(inputNames...))
		if err != nil {
			return Value{}, err
		}
		defer program.Close()
		return program.run(ctx, values, config)
	}
	ctx, config, err = prepareRun(ctx, config)
	if err != nil {
		return Value{}, err
	}
	var arena rawArena
	nameRefs := make([]ffi.Str, 0, len(values))
	raw := make([]ffi.RawValue, 0, len(values))
	// Inputs whose Kind has no inline raw form (dates, paths, dataclasses, ...)
	// are converted to owned Rust handles, which arena.ownsHandles records.
	// Rust's read_raw_value consumes those handles in place on a successful
	// read — nulling each slot — so freeing here is a no-op on success but
	// reclaims handles Rust never read when conversion or compilation fails.
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
	var token uintptr
	if ctx.Done() != nil {
		if token, err = ffi.CancelTokenNew(); err == nil {
			defer ffi.CancelTokenFree(token)
		} else {
			token = 0
		}
	}
	args := ffi.CompileRunFastRawArgs{
		Code:            ffi.StringRef(code),
		ScriptName:      ffi.StringRef(evalScriptName),
		InputNames:      slicePtr(nameRefs),
		InputCount:      uintptr(len(nameRefs)),
		InputValues:     slicePtr(raw),
		InputValueCount: uintptr(len(raw)),
		Limits:          config.ffiLimits(token),
	}
	output := runFastOutputPool.Get().(*ffi.RunFastOutput) //nolint:errcheck // pool only stores *ffi.RunFastOutput
	defer runFastOutputPool.Put(output)
	stop := maybeWatchCancel(ctx, token)
	callErr := ffi.ProgramCompileRunFastRaw(&args, output)
	stop()
	runtime.KeepAlive(values)
	runtime.KeepAlive(&arena)
	if err := p0FinishEval(ctx, config, output, callErr); err != nil {
		return Value{}, err
	}
	return decodeFastOutput(output)
}

// p0FinishEval mirrors finishFastOutput for the program-less Eval path.
func p0FinishEval(ctx context.Context, config runConfig, output *ffi.RunFastOutput, callErr error) error {
	printed := ffi.TakePrinted(output.Print, output.PrintFlags)
	output.Print = ffi.Bytes{}
	writeErr := writePrint(&config, printed)
	if callErr != nil {
		ffi.RawValueFree(&output.Value)
		freeFastOutputBytes(output)
		return errors.Join(wrapCtxErrorBound(ctx, compileError(callErr), config.deadlineBound), writeErr)
	}
	if writeErr != nil {
		ffi.RawValueFree(&output.Value)
		freeFastOutputBytes(output)
		return writeErr
	}
	return nil
}

// EvalAs combines Eval with As so callers get a typed result in one line.
func EvalAs[T any](ctx context.Context, code string, inputs any, opts ...RunOption) (T, error) {
	var zero T
	value, err := Eval(ctx, code, inputs, opts...)
	if err != nil {
		return zero, err
	}
	return As[T](value)
}

// --------------------------------------------------------------------------
// Shared run plumbing
// --------------------------------------------------------------------------

func prepareRun(ctx context.Context, config runConfig) (context.Context, runConfig, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ctx, config, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		config.limits, config.deadlineBound = limitsWithContextDeadline(config.limits, deadline)
	}
	return ctx, config, nil
}

func (p *Program) runConfig(opts ...RunOption) runConfig {
	config := runConfig{functions: p.functions}
	for _, opt := range opts {
		opt.applyRun(&config)
	}
	return config
}

// validateForProgram rejects REPL-only options on Program runs.
func (c *runConfig) validateForProgram() error {
	if c.skipTypeCheck {
		return fmt.Errorf("monty: WithoutTypeCheck applies only to REPL snippets")
	}
	return nil
}

// needsHostPath reports whether the run requires dispatch or streaming that
// the plain direct path cannot provide.
func (c *runConfig) needsHostPath() bool {
	return len(c.functions) != 0 || c.osHandler != nil || len(c.mounts) != 0 ||
		len(c.fsMounts) != 0 || c.stdout != nil || c.stderr != nil
}

// hostPathEligible reports whether every dispatch need can be served inside a
// single host-callback FFI hop (everything except async functions and
// suspendable execution).
func (c *runConfig) hostPathEligible() bool {
	for _, function := range c.functions {
		if function == nil || function.async {
			return false
		}
	}
	return true
}

func (c *runConfig) ffiLimits(token uintptr) *ffi.Limits {
	if c.limits == nil && token == 0 {
		return nil
	}
	var limits *ffi.Limits
	if c.limits != nil {
		limits = c.limits.ffi()
	} else {
		limits = &ffi.Limits{}
	}
	limits.CancelToken = token
	return limits
}

func (c *runConfig) functionNameRefs(p *Program) []ffi.Str {
	if p != nil && !c.functionsOwned && len(p.funcNames) != 0 {
		return p.funcNames
	}
	return hostFunctionNameRefs(c.functions)
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

func slicePtr[T any](values []T) unsafe.Pointer {
	if len(values) == 0 {
		return nil
	}
	return unsafe.Pointer(unsafe.SliceData(values))
}

type rawInputKeepAlive struct {
	inputValues map[string]Value
	arena       rawArena
}

func (k rawInputKeepAlive) release(raw []ffi.RawValue) {
	if k.arena.ownsHandles {
		freeOwnedRawValues(raw)
	}
}

func (p *Program) rawInputs(inputs any) ([]ffi.RawValue, rawInputKeepAlive, error) {
	inputValues, err := p.convertInputs(inputs)
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

// convertInputs normalizes inputs, encoding values whose Go types are bound
// by WithDataclasses as dataclass instances.
func (p *Program) convertInputs(inputs any) (map[string]Value, error) {
	if len(p.dataclasses) != 0 {
		if named, ok := inputs.(map[string]any); ok {
			converted := make(map[string]Value, len(named))
			for name, item := range named {
				value, err := p.convertInput(item)
				if err != nil {
					return nil, fmt.Errorf("monty: input %q: %w", name, err)
				}
				converted[name] = value
			}
			return converted, nil
		}
	}
	return inputsToValues(inputs)
}

func (p *Program) convertInput(item any) (Value, error) {
	if item != nil {
		itemType := reflect.TypeOf(item)
		for _, dataclass := range p.dataclasses {
			if dataclass.matches(itemType) {
				return dataclass.Wrap(item)
			}
		}
	}
	return From(item)
}

// callLocked runs fn under the program read lock with the program handle.
// It releases input keep-alive state after fn returns. A zero handle
// short-circuits with ErrClosed.
func (p *Program) callLocked(rawInputs []ffi.RawValue, keepAlive rawInputKeepAlive, fn func(handle uintptr) error) error {
	p.mu.RLock()
	if p.handle == 0 {
		p.mu.RUnlock()
		keepAlive.release(rawInputs)
		return ErrClosed
	}
	err := fn(p.handle)
	p.mu.RUnlock()
	keepAlive.release(rawInputs)
	runtime.KeepAlive(keepAlive)
	return err
}

// --------------------------------------------------------------------------
// Host callback (single-hop dispatch)
// --------------------------------------------------------------------------

var (
	hostCallbackPtr  = ffi.NewCallback(hostCallCallback)
	printCallbackPtr = ffi.NewCallback(printCallback)
)

// hostCallbackState carries one run's dispatch context across the Rust
// callback boundary. Strings and values written into callback outputs are
// parked here so they stay reachable until Rust copies them (synchronously,
// before the next callback or return).
type hostCallbackState struct {
	ctx       context.Context
	functions map[string]*Function
	osHandler OSHandler
	fsMounts  []fsMount
	stdout    io.Writer
	stderr    io.Writer
	writeErr  error

	excType  string
	message  string
	retained Value
	arena    *rawArena
}

func (s *hostCallbackState) reset(ctx context.Context, config *runConfig) {
	*s = hostCallbackState{
		ctx:       ctx,
		functions: config.functions,
		osHandler: config.osHandler,
		fsMounts:  config.fsMounts,
		stdout:    config.stdout,
		stderr:    config.stderr,
	}
}

// clear drops references so the pooled state does not pin a finished run's
// context, functions, or parked values.
func (s *hostCallbackState) clear() {
	*s = hostCallbackState{}
}

// hostRunScratch bundles the per-call FFI argument block and callback state
// in one pooled allocation. Both must be heap-resident: their addresses cross
// the raw trampoline as bare uintptrs.
type hostRunScratch struct {
	args  ffi.RunHostArgs
	state hostCallbackState
}

var hostScratchPool = sync.Pool{
	New: func() any { return new(hostRunScratch) },
}

func hostCallCallback(userData unsafe.Pointer, kind uint32, namePtr unsafe.Pointer, nameLen uintptr, argsPtr, kwargsPtr, outPtr unsafe.Pointer) (status int32) {
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
	// FFI call. A panic from user handler code would unwind across the Rust
	// frames, which is undefined behavior and crashes the process. Convert
	// any panic into a Python exception instead.
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
	if argsPtr == nil || kwargsPtr == nil {
		return state.writeException(out, "RuntimeError", "host callback args pointer is null")
	}
	args := (*ffi.RawValue)(argsPtr)
	kwargs := (*ffi.RawValue)(kwargsPtr)
	if kind == ffi.HostCallOS {
		return state.dispatchOSCall(out, name, args, kwargs)
	}
	return state.dispatchFunctionCall(out, name, args, kwargs)
}

func (s *hostCallbackState) dispatchFunctionCall(out *ffi.HostFunctionOutput, name string, args, kwargs *ffi.RawValue) int32 {
	function := s.functions[name]
	if function == nil {
		return s.writeException(out, "NameError", fmt.Sprintf("name %q is not defined", name))
	}
	if function.fastRawCall != nil {
		raw, err := function.fastRawCall(s.ctx, *args, *kwargs)
		if err != nil {
			excType, message := exceptionFromError(err)
			return s.writeException(out, excType, message)
		}
		out.Value = raw
		return ffi.HostCallbackReturn
	}
	argValues, kwargValues, err := decodeBorrowedCall(args, kwargs)
	if err != nil {
		return s.writeException(out, "RuntimeError", err.Error())
	}
	result, err := function.call(s.ctx, argValues, kwargValues)
	if err != nil {
		excType, message := exceptionFromError(err)
		return s.writeException(out, excType, message)
	}
	return s.writeValue(out, result)
}

func (s *hostCallbackState) dispatchOSCall(out *ffi.HostFunctionOutput, name string, args, kwargs *ffi.RawValue) int32 {
	if len(s.fsMounts) == 0 && s.osHandler == nil {
		return ffi.HostCallbackNotHandled
	}
	argValues, kwargValues, err := decodeBorrowedCall(args, kwargs)
	if err != nil {
		return s.writeException(out, "RuntimeError", err.Error())
	}
	result, err := dispatchOSCall(s.ctx, s.fsMounts, s.osHandler, OSFunction(name), argValues, kwargValues)
	switch {
	case errors.Is(err, ErrNotHandled):
		return ffi.HostCallbackNotHandled
	case err != nil:
		excType, message := exceptionFromError(err)
		return s.writeException(out, excType, message)
	default:
		return s.writeValue(out, result)
	}
}

// writeValue encodes a host result into the callback output, parking the
// value (and any owned-raw arena) on the state so its memory survives until
// Rust reads it.
func (s *hostCallbackState) writeValue(out *ffi.HostFunctionOutput, value Value) int32 {
	s.arena = &rawArena{}
	raw, err := valueToRaw(value, s.arena)
	if err != nil {
		excType, message := exceptionFromError(err)
		return s.writeException(out, excType, message)
	}
	s.retained = value
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

// printCallback streams one flushed print fragment to the configured writers.
func printCallback(userData unsafe.Pointer, stream uint8, ptr unsafe.Pointer, length uintptr) uintptr {
	if userData == nil || ptr == nil || length == 0 {
		return 0
	}
	state := (*hostCallbackState)(userData)
	// Like hostCallCallback, this runs inside an extern "C" frame: a panic
	// from a user-supplied Writer must not unwind across the Rust frames.
	// Record it as the run's write error instead.
	defer func() {
		if r := recover(); r != nil {
			state.writeErr = fmt.Errorf("monty: print writer panicked: %v", r)
		}
	}()
	if state.writeErr != nil {
		return 0
	}
	text := unsafe.String((*byte)(ptr), int(length))
	writer := state.stdout
	if stream == ffi.StreamStderr {
		writer = state.stderr
	}
	if writer == nil {
		return 0
	}
	if _, err := io.WriteString(writer, text); err != nil {
		state.writeErr = err
	}
	return 0
}

// decodeBorrowedCall converts callback args/kwargs into Values without
// consuming the Rust-owned raw trees (the caller frees them after the
// callback returns).
func decodeBorrowedCall(args, kwargs *ffi.RawValue) ([]Value, map[string]Value, error) {
	argList, err := decodeBorrowedRawValue(args)
	if err != nil {
		return nil, nil, err
	}
	if argList.kind != ListKind {
		return nil, nil, fmt.Errorf("monty: callback args are %s, not list", argList.kind)
	}
	kwargDict, err := decodeBorrowedRawValue(kwargs)
	if err != nil {
		return nil, nil, err
	}
	if kwargDict.kind != DictKind {
		return nil, nil, fmt.Errorf("monty: callback kwargs are %s, not dict", kwargDict.kind)
	}
	var kwargValues map[string]Value
	if len(kwargDict.pairs) != 0 {
		kwargValues = make(map[string]Value, len(kwargDict.pairs))
		for i := range kwargDict.pairs {
			pair := &kwargDict.pairs[i]
			if pair.Key.kind != StringKind {
				return nil, nil, fmt.Errorf("monty: keyword argument key is %s, not string", pair.Key.kind)
			}
			kwargValues[pair.Key.text] = pair.Value
		}
	}
	return argList.items, kwargValues, nil
}

// decodeBorrowedRawValue reads a raw value tree without taking ownership:
// buffers are copied, child arrays are recursed, and value handles are read
// but never freed.
func decodeBorrowedRawValue(raw *ffi.RawValue) (Value, error) {
	kind := Kind(raw.Kind)
	switch kind {
	case EllipsisKind:
		return Ellipsis(), nil
	case NoneKind:
		return None(), nil
	case BoolKind:
		return Bool(raw.Bool != 0), nil
	case IntKind:
		return Int64(raw.Int), nil
	case FloatKind:
		return Float(raw.Float), nil
	case StringKind, BigIntKind, PathKind, ReprKind, CycleKind, FunctionKind, TypeKind, BuiltinFunctionKind, ExceptionKind:
		text := ""
		if raw.Ptr != nil && raw.Len != 0 {
			text = string(unsafe.Slice((*byte)(raw.Ptr), raw.Len))
		}
		if kind == ExceptionKind {
			if excType, message, ok := strings.Cut(text, ": "); ok && excType != "" {
				return exceptionValue(excType, message), nil
			}
			return exceptionValue(text, ""), nil
		}
		return Value{kind: kind, text: text}, nil
	case BytesKind:
		var data []byte
		if raw.Ptr != nil && raw.Len != 0 {
			data = append([]byte(nil), unsafe.Slice((*byte)(raw.Ptr), raw.Len)...)
		}
		return Value{kind: BytesKind, bytes: data}, nil
	case ListKind, TupleKind, SetKind, FrozenSetKind:
		if raw.Handle != 0 {
			return decodeValue(raw.Handle)
		}
		count := int(raw.Len)
		if count == 0 {
			return Value{kind: kind}, nil
		}
		if raw.Ptr == nil {
			return Value{}, fmt.Errorf("monty: raw %s value has null item pointer", kind)
		}
		children := unsafe.Slice((*ffi.RawValue)(raw.Ptr), count)
		items := make([]Value, count)
		for i := range children {
			item, err := decodeBorrowedRawValue(&children[i])
			if err != nil {
				return Value{}, err
			}
			items[i] = item
		}
		return Value{kind: kind, items: items}, nil
	case DictKind:
		if raw.Handle != 0 {
			return decodeValue(raw.Handle)
		}
		count := int(raw.Len)
		if count == 0 {
			return Value{kind: kind}, nil
		}
		if raw.Ptr == nil {
			return Value{}, fmt.Errorf("monty: raw %s value has null pair pointer", kind)
		}
		children := unsafe.Slice((*ffi.RawPair)(raw.Ptr), count)
		pairs := make([]Pair, count)
		for i := range children {
			key, err := decodeBorrowedRawValue(&children[i].Key)
			if err != nil {
				return Value{}, err
			}
			value, err := decodeBorrowedRawValue(&children[i].Value)
			if err != nil {
				return Value{}, err
			}
			pairs[i] = Pair{Key: key, Value: value}
		}
		return Value{kind: kind, pairs: pairs}, nil
	default:
		if raw.Handle != 0 {
			return decodeValue(raw.Handle)
		}
		return Value{}, fmt.Errorf("monty: raw %s value did not include a value handle", kind)
	}
}
