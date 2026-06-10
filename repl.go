package monty

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"github.com/joeychilson/monty/internal/ffi"
)

// REPL is a stateful Python session: globals and heap state persist across
// snippets. Methods are safe for concurrent use but execute one snippet at a
// time.
type REPL struct {
	mu      sync.Mutex
	handle  uintptr
	cleanup runtime.Cleanup
	closed  bool
	// busy marks a Start-run mid-snippet: the session moved into the Run and
	// comes back when it completes or fails.
	busy bool
	// lost marks a session that will never come back: its in-flight run
	// ended (failed or was closed while paused) without returning it.
	lost bool

	config       replConfig
	functions    map[string]*Function
	typeSnippets []string
}

// replFeedScratch bundles the feed argument block and callback state in one
// pooled allocation. Both must be heap-resident: their addresses cross the
// raw FFI trampoline as bare uintptrs, so a stack-allocated copy could be
// moved mid-call by a stack growth while Rust still holds the old address
// (see hostRunScratch).
type replFeedScratch struct {
	args  ffi.ReplFeedArgs
	state hostCallbackState
}

var replFeedScratchPool = sync.Pool{
	New: func() any { return new(replFeedScratch) },
}

// NewREPL creates an empty session. Session options set defaults applied to
// every snippet: script name, limits, functions, type checking, stubs,
// dataclasses, and print writers.
func NewREPL(opts ...REPLOption) (*REPL, error) {
	config := replConfig{scriptName: "main.py"}
	for _, opt := range opts {
		opt.applyREPL(&config)
	}
	functions := make(map[string]*Function, len(config.functions))
	for _, function := range config.functions {
		if _, exists := functions[function.Name()]; exists {
			return nil, fmt.Errorf("monty: duplicate function %q", function.Name())
		}
		functions[function.Name()] = function
	}
	limits := Limits{}
	if config.limits != nil {
		limits = *config.limits
	}
	// Force a tracked session by attaching a (never-cancelled) token at
	// construction: per-snippet duration budgets and context cancellation
	// require the tracker, and upstream fixes the tracker type when the
	// session is created.
	token, err := ffi.CancelTokenNew()
	if err != nil {
		return nil, err
	}
	defer ffi.CancelTokenFree(token)
	ffiLimits := limits.ffi()
	ffiLimits.CancelToken = token
	handle, err := ffi.ReplNew(config.scriptName, ffiLimits)
	if err != nil {
		return nil, normalizeError(err)
	}
	repl := &REPL{handle: handle, config: config, functions: functions}
	repl.cleanup = runtime.AddCleanup(repl, ffi.ReplFree, handle)
	return repl, nil
}

// LoadREPL restores a session serialized by REPL.Dump. Session defaults
// (functions, writers, type checking) are not serialized; re-provide them
// here.
func LoadREPL(data []byte, opts ...REPLOption) (*REPL, error) {
	payload, err := unwrapSnapshot(data, snapshotKindREPL)
	if err != nil {
		return nil, err
	}
	config := replConfig{scriptName: "main.py"}
	for _, opt := range opts {
		opt.applyREPL(&config)
	}
	functions := make(map[string]*Function, len(config.functions))
	for _, function := range config.functions {
		functions[function.Name()] = function
	}
	handle, err := ffi.ReplLoad(payload)
	if err != nil {
		return nil, normalizeError(err)
	}
	repl := &REPL{handle: handle, config: config, functions: functions}
	repl.cleanup = runtime.AddCleanup(repl, ffi.ReplFree, handle)
	return repl, nil
}

// LoadREPLRun restores a mid-snippet execution serialized by Run.Dump on a
// REPL run. It returns both the paused Run and the session it belongs to;
// the session becomes usable when the Run completes or fails.
func LoadREPLRun(data []byte, opts ...RunOption) (*REPL, *Run, error) {
	payload, err := unwrapSnapshot(data, snapshotKindRun)
	if err != nil {
		return nil, nil, err
	}
	handle, err := ffi.ProgressLoad(payload)
	if err != nil {
		return nil, nil, normalizeError(err)
	}
	if !ffi.ProgressIsRepl(handle) {
		ffi.ProgressFree(handle)
		return nil, nil, fmt.Errorf("monty: snapshot is a program execution; load it with LoadRun")
	}
	repl := &REPL{busy: true, config: replConfig{scriptName: "main.py"}, functions: map[string]*Function{}}
	run, err := runFromLoadedHandle(handle, repl, opts...)
	if err != nil {
		return nil, nil, err
	}
	return repl, run, nil
}

// Close releases the session. Close is idempotent.
func (r *REPL) Close() {
	if r == nil {
		return
	}
	// cleanup is written by adoptHandle under the mutex; stopping it outside
	// the lock would race a finishing Run re-registering it.
	r.mu.Lock()
	r.cleanup.Stop()
	handle := r.handle
	r.handle = 0
	r.closed = true
	r.mu.Unlock()
	ffi.ReplFree(handle)
}

// Dump serializes the session (globals, heap, defined functions) so it can
// be restored with LoadREPL, possibly in another process.
func (r *REPL) Dump() ([]byte, error) {
	if r == nil {
		return nil, ErrClosed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.usableLocked(); err != nil {
		return nil, err
	}
	payload, err := ffi.ReplDump(r.handle)
	if err != nil {
		return nil, normalizeError(err)
	}
	return wrapSnapshot(snapshotKindREPL, payload), nil
}

func (r *REPL) usableLocked() error {
	if r.lost {
		return fmt.Errorf("monty: REPL session lost: its snippet run ended without handing the session back: %w", ErrClosed)
	}
	if r.closed || r.handle == 0 && !r.busy {
		return ErrClosed
	}
	if r.busy {
		return ErrBusy
	}
	return nil
}

// sessionLost marks the session unrecoverable: the Run holding it reached a
// terminal state without handing it back. No-op once the session returned.
func (r *REPL) sessionLost() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.busy {
		return
	}
	r.busy = false
	r.lost = true
}

// adoptHandle installs the session handle returned by a completed or failed
// REPL run, replacing the consumed one.
func (r *REPL) adoptHandle(handle uintptr) {
	r.mu.Lock()
	old := r.handle
	r.handle = handle
	r.busy = false
	closed := r.closed
	r.cleanup.Stop()
	if !closed {
		r.cleanup = runtime.AddCleanup(r, ffi.ReplFree, handle)
	}
	r.mu.Unlock()
	ffi.ReplFree(old)
	if closed {
		ffi.ReplFree(handle)
	}
}

// recordSnippet appends a successfully executed snippet to the accumulated
// type-check context.
func (r *REPL) recordSnippet(code string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.typeSnippets = append(r.typeSnippets, code)
}

// snippetRunConfig merges session defaults with per-snippet options.
func (r *REPL) snippetRunConfig(opts []RunOption) runConfig {
	config := runConfig{
		limits:    r.config.limits,
		stdout:    r.config.stdout,
		stderr:    r.config.stderr,
		functions: r.functions,
	}
	for _, opt := range opts {
		opt.applyRun(&config)
	}
	return config
}

// typeCheckCode statically checks code in the session's module context.
//
// Genuine external declarations — explicit WithStubs text plus function and
// dataclass signatures — go through the stubs channel, which gives them
// declaration semantics (their declared types are fixed, as a .pyi stub
// intends). Previously executed snippets are instead prepended into the
// snippet's own module so that unannotated rebinding and narrowing stay legal,
// exactly as they are in a single continuous module. Routing history through
// the stubs channel would freeze each history binding's declared type and
// reject ordinary REPL reassignment (e.g. "x = 1" then "x = x + 1").
//
// Prepending shifts the snippet off line 1, so the resulting diagnostics are
// remapped back to snippet-relative coordinates by newTypeCheckErrorOffset.
func (r *REPL) typeCheckCode(code string, functions map[string]*Function, extraStubs string) error {
	stubs := joinStubs(r.config.stubs, functions, r.config.dataclasses)
	if strings.TrimSpace(extraStubs) != "" {
		if strings.TrimSpace(stubs) != "" {
			stubs += "\n\n" + extraStubs
		} else {
			stubs = extraStubs
		}
	}

	r.mu.Lock()
	history := strings.Join(r.typeSnippets, "\n\n")
	r.mu.Unlock()

	module, offset := code, 0
	if history != "" {
		module = history + "\n" + code
		offset = strings.Count(history, "\n") + 1
	}

	diags, err := ffi.TypeCheck(module, r.config.scriptName, stubs, "type_stubs.pyi")
	if err != nil {
		return execError(err)
	}
	if diags != 0 {
		if tcErr := newTypeCheckErrorOffset(diags, offset); tcErr != nil {
			return tcErr
		}
	}
	return nil
}

// TypeCheck statically checks a snippet against the session's stubs and
// accumulated history without executing it. It returns nil or a
// *TypeCheckError.
func (r *REPL) TypeCheck(code string, extraStubs ...string) error {
	if r == nil {
		return ErrClosed
	}
	return r.typeCheckCode(code, r.functions, strings.Join(extraStubs, "\n\n"))
}

// Eval executes one snippet against the persistent session state and returns
// its value. Per-snippet options may add functions, mounts, WithFS
// filesystems, an OS handler, print writers, and a MaxDuration budget.
func (r *REPL) Eval(ctx context.Context, code string, inputs any, opts ...RunOption) (Value, error) {
	if r == nil {
		return Value{}, ErrClosed
	}
	config := r.snippetRunConfig(opts)
	ctx, config, err := prepareRun(ctx, config)
	if err != nil {
		return Value{}, err
	}
	if err := config.openMounts(); err != nil {
		return Value{}, err
	}
	if r.config.typeCheck && !config.skipTypeCheck {
		if err := r.typeCheckSnippet(code, config.functions); err != nil {
			return Value{}, err
		}
	}
	names, values, arena, err := replInputs(inputs)
	if err != nil {
		return Value{}, err
	}
	defer func() {
		if arena.ownsHandles {
			freeOwnedRawValues(values)
		}
	}()

	var token uintptr
	if ctx.Done() != nil {
		if token, err = ffi.CancelTokenNew(); err == nil {
			defer ffi.CancelTokenFree(token)
		} else {
			token = 0
		}
	}
	scratch := replFeedScratchPool.Get().(*replFeedScratch) //nolint:errcheck // pool only stores *replFeedScratch
	state := &scratch.state
	defer func() {
		state.clear()
		replFeedScratchPool.Put(scratch)
	}()
	state.reset(ctx, &config)
	mounts := config.mountFFIHandles()
	funcNames := hostFunctionNameRefs(config.functions)
	nameRefs := ffi.StringRefs(names)
	scratch.args = ffi.ReplFeedArgs{
		Code:          ffi.StringRef(code),
		InputNames:    slicePtr(nameRefs),
		InputValues:   slicePtr(values),
		InputCount:    uintptr(len(values)),
		CancelToken:   token,
		HostNames:     slicePtr(funcNames),
		HostNameCount: uintptr(len(funcNames)),
		Mounts:        slicePtr(mounts),
		MountCount:    uintptr(len(mounts)),
		Callback:      hostCallbackPtr,
		CallbackData:  uintptr(unsafe.Pointer(state)),
	}
	if config.limits != nil && config.limits.MaxDuration > 0 {
		scratch.args.HasMaxDuration = 1
		scratch.args.MaxDurationNanos = uint64(config.limits.MaxDuration)
	}
	if config.stdout != nil || config.stderr != nil {
		scratch.args.Print = printCallbackPtr
		scratch.args.PrintData = uintptr(unsafe.Pointer(state))
	}

	output := runFastOutputPool.Get().(*ffi.RunFastOutput) //nolint:errcheck // pool only stores *ffi.RunFastOutput
	defer runFastOutputPool.Put(output)

	r.mu.Lock()
	if err := r.usableLocked(); err != nil {
		r.mu.Unlock()
		return Value{}, err
	}
	handle := r.handle
	stop := maybeWatchCancel(ctx, token)
	callErr := ffi.ReplFeedRunRaw(handle, &scratch.args, output)
	stop()
	r.mu.Unlock()
	runtime.KeepAlive(nameRefs)
	runtime.KeepAlive(names)
	runtime.KeepAlive(funcNames)
	runtime.KeepAlive(mounts)
	runtime.KeepAlive(scratch)
	runtime.KeepAlive(&arena)

	printed := ffi.TakePrinted(output.Print, output.PrintFlags)
	output.Print = ffi.Bytes{}
	writeErr := writePrint(&config, printed)
	if writeErr == nil {
		writeErr = state.writeErr
	}
	if callErr != nil {
		ffi.RawValueFree(&output.Value)
		freeFastOutputBytes(output)
		return Value{}, errors.Join(wrapCtxErrorBound(ctx, execError(callErr), config.deadlineBound), writeErr)
	}
	if writeErr != nil {
		ffi.RawValueFree(&output.Value)
		freeFastOutputBytes(output)
		return Value{}, writeErr
	}
	value, err := decodeFastOutput(output)
	if err != nil {
		return Value{}, err
	}
	if r.config.typeCheck && !config.skipTypeCheck {
		r.recordSnippet(code)
	}
	return value, nil
}

func (r *REPL) typeCheckSnippet(code string, functions map[string]*Function) error {
	return r.typeCheckCode(code, functions, "")
}

// Start begins executing one snippet as a suspendable Run. The session moves
// into the Run while the snippet is in flight (other REPL methods return
// ErrBusy) and is handed back when the Run completes or fails; closing a
// paused REPL Run abandons the session state.
func (r *REPL) Start(ctx context.Context, code string, inputs any, opts ...RunOption) (*Run, error) {
	if r == nil {
		return nil, ErrClosed
	}
	config := r.snippetRunConfig(opts)
	ctx, config, err := prepareRun(ctx, config)
	if err != nil {
		return nil, err
	}
	if err := config.openMounts(); err != nil {
		return nil, err
	}
	if r.config.typeCheck && !config.skipTypeCheck {
		if err := r.typeCheckSnippet(code, config.functions); err != nil {
			return nil, err
		}
	}
	names, values, arena, err := replInputs(inputs)
	if err != nil {
		return nil, err
	}
	defer func() {
		if arena.ownsHandles {
			freeOwnedRawValues(values)
		}
	}()

	run := &Run{state: statePaused, config: config, repl: r}
	if ctx.Done() != nil {
		if token, tokenErr := ffi.CancelTokenNew(); tokenErr == nil {
			run.cancelToken = token
		}
	}
	if r.config.typeCheck && !config.skipTypeCheck {
		run.typeSnippet = code
	}
	nameRefs := ffi.StringRefs(names)
	scratch := replFeedScratchPool.Get().(*replFeedScratch) //nolint:errcheck // pool only stores *replFeedScratch
	defer replFeedScratchPool.Put(scratch)
	scratch.args = ffi.ReplFeedArgs{
		Code:        ffi.StringRef(code),
		InputNames:  slicePtr(nameRefs),
		InputValues: slicePtr(values),
		InputCount:  uintptr(len(values)),
		CancelToken: run.cancelToken,
	}
	if config.limits != nil && config.limits.MaxDuration > 0 {
		scratch.args.HasMaxDuration = 1
		scratch.args.MaxDurationNanos = uint64(config.limits.MaxDuration)
	}

	r.mu.Lock()
	if err := r.usableLocked(); err != nil {
		r.mu.Unlock()
		run.releaseCancelTokenLocked()
		return nil, err
	}
	handle := r.handle
	r.busy = true
	stop := run.watchCancelLocked(ctx)
	step, callErr := ffi.ReplFeedStart(handle, &scratch.args)
	stop()
	r.mu.Unlock()
	runtime.KeepAlive(scratch)
	runtime.KeepAlive(nameRefs)
	runtime.KeepAlive(names)
	runtime.KeepAlive(&arena)

	run.mu.Lock()
	run.advanceLocked(ctx, step, callErr)
	run.mu.Unlock()
	return run, nil
}

// Call invokes a Python function defined in the session by name.
func (r *REPL) Call(ctx context.Context, name string, args ...any) (Value, error) {
	if r == nil {
		return Value{}, ErrClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Value{}, err
	}
	arena := &rawArena{}
	rawArgs := make([]ffi.RawValue, len(args))
	for i, arg := range args {
		value, err := From(arg)
		if err != nil {
			freeOwnedRawValues(rawArgs)
			return Value{}, fmt.Errorf("monty: argument %d: %w", i, err)
		}
		raw, err := valueToRaw(value, arena)
		if err != nil {
			freeOwnedRawValues(rawArgs)
			return Value{}, err
		}
		rawArgs[i] = raw
	}
	output := runFastOutputPool.Get().(*ffi.RunFastOutput) //nolint:errcheck // pool only stores *ffi.RunFastOutput
	defer runFastOutputPool.Put(output)

	r.mu.Lock()
	if err := r.usableLocked(); err != nil {
		r.mu.Unlock()
		freeOwnedRawValues(rawArgs)
		return Value{}, err
	}
	callErr := ffi.ReplCallRaw(r.handle, name, rawArgs, output)
	r.mu.Unlock()
	runtime.KeepAlive(arena)
	if arena.ownsHandles {
		freeOwnedRawValues(rawArgs)
	}

	printed := ffi.TakePrinted(output.Print, output.PrintFlags)
	output.Print = ffi.Bytes{}
	config := runConfig{stdout: r.config.stdout, stderr: r.config.stderr}
	writeErr := writePrint(&config, printed)
	if callErr != nil {
		ffi.RawValueFree(&output.Value)
		freeFastOutputBytes(output)
		return Value{}, execError(callErr)
	}
	if writeErr != nil {
		ffi.RawValueFree(&output.Value)
		freeFastOutputBytes(output)
		return Value{}, writeErr
	}
	return decodeFastOutput(output)
}

// FunctionNames lists the Python functions defined in the session.
func (r *REPL) FunctionNames() ([]string, error) {
	if r == nil {
		return nil, ErrClosed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.usableLocked(); err != nil {
		return nil, err
	}
	names, err := ffi.ReplFunctionNames(r.handle)
	return names, normalizeError(err)
}

// HasFunction reports whether the session defines a Python function.
func (r *REPL) HasFunction(name string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.usableLocked() != nil {
		return false
	}
	return ffi.ReplHasFunction(r.handle, name)
}

// replInputs converts the inputs argument into parallel name/value arrays
// for the FFI feed calls.
func replInputs(inputs any) ([]string, []ffi.RawValue, *rawArena, error) {
	values, err := inputsToValues(inputs)
	if err != nil {
		return nil, nil, nil, err
	}
	arena := &rawArena{}
	if len(values) == 0 {
		return nil, nil, arena, nil
	}
	names := make([]string, 0, len(values))
	raw := make([]ffi.RawValue, 0, len(values))
	for name, value := range values {
		converted, err := valueToRaw(value, arena)
		if err != nil {
			freeOwnedRawValues(raw)
			return nil, nil, nil, err
		}
		names = append(names, name)
		raw = append(raw, converted)
	}
	return names, raw, arena, nil
}

// --------------------------------------------------------------------------
// Continuation detection
// --------------------------------------------------------------------------

// ContinuationMode reports whether interactive source is complete enough to
// execute.
type ContinuationMode uint32

const (
	// ContinuationComplete means the source parses as a complete snippet.
	ContinuationComplete ContinuationMode = iota
	// ContinuationImplicit means the source ends inside an implicit
	// continuation (open bracket, trailing operator).
	ContinuationImplicit
	// ContinuationBlock means the source ends inside an indented block.
	ContinuationBlock
)

// String returns a stable display name for the mode.
func (m ContinuationMode) String() string {
	switch m {
	case ContinuationComplete:
		return "complete"
	case ContinuationImplicit:
		return "incomplete-implicit"
	case ContinuationBlock:
		return "incomplete-block"
	default:
		return fmt.Sprintf("ContinuationMode(%d)", uint32(m))
	}
}

// DetectContinuation classifies interactive source for REPL line editors.
func DetectContinuation(code string) (ContinuationMode, error) {
	mode, err := ffi.ReplContinuationMode(code)
	if err != nil {
		return ContinuationComplete, err
	}
	return ContinuationMode(mode), nil
}
