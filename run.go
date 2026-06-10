package monty

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"

	"github.com/joeychilson/monty/internal/ffi"
)

// CallID correlates a deferred external call with its future resolution.
type CallID uint32

// Interrupt is a reason execution paused, surfaced by Run.Pending. The sealed
// set is *Call, *NameLookup, and *Gather; each carries the resume methods
// valid for its state, and resuming advances the owning Run in place.
type Interrupt interface{ isInterrupt() }

// Call is a paused external call: a host function call (OS false) or an OS
// operation such as "Path.read_text" (OS true).
type Call struct {
	// Name is the function name or OS function literal.
	Name string
	// Args are the positional Python arguments.
	Args []Value
	// Kwargs are the keyword Python arguments.
	Kwargs map[string]Value
	// ID correlates the call when it is deferred to a future.
	ID CallID
	// OS reports whether this is an OS-level call.
	OS bool
	// Method reports a dataclass method call (Args[0] is self).
	Method bool

	run *Run
}

func (*Call) isInterrupt() {}

// Return resumes execution with the call's result, converted with the same
// rules as From.
func (c *Call) Return(ctx context.Context, result any) error {
	if c == nil || c.run == nil {
		return ErrResolved
	}
	value, err := From(result)
	if err != nil {
		return err
	}
	return c.run.resolveWithValue(ctx, c, value, ffi.ProgressResumeReturnRaw)
}

// Raise resumes execution by raising err inside Python: a *Exception (or
// *ExecError) keeps its exception type, anything else raises RuntimeError.
func (c *Call) Raise(ctx context.Context, err error) error {
	if c == nil || c.run == nil {
		return ErrResolved
	}
	excType, message := exceptionFromError(err)
	return c.run.resolve(ctx, c, func(handle uintptr) (ffi.StepResult, error) {
		return ffi.ProgressResumeException(handle, excType, message)
	})
}

// Defer resumes execution with a pending future in place of a result; resolve
// it later through the *Gather interrupt using this call's ID. Defer is only
// valid for host function calls, not OS calls.
func (c *Call) Defer(ctx context.Context) error {
	if c == nil || c.run == nil {
		return ErrResolved
	}
	if c.OS {
		return fmt.Errorf("monty: Defer is only valid for function calls; resolve OS calls with Return, Raise, or NotHandled")
	}
	return c.run.resolve(ctx, c, ffi.ProgressResumePending)
}

// NotHandled resumes an OS call with Monty's default unhandled behavior
// (PermissionError for filesystem operations, RuntimeError otherwise).
func (c *Call) NotHandled(ctx context.Context) error {
	if c == nil || c.run == nil {
		return ErrResolved
	}
	if !c.OS {
		return fmt.Errorf("monty: NotHandled is only valid for OS calls")
	}
	return c.run.resolve(ctx, c, ffi.ProgressResumeNotHandled)
}

// NameLookup is a paused lookup of an unknown Python global name.
type NameLookup struct {
	// Name is the global being resolved.
	Name string

	run *Run
}

func (*NameLookup) isInterrupt() {}

// Return provides the value for the name (a *Function or
// ExternalFunction value works for callables) and resumes execution.
func (n *NameLookup) Return(ctx context.Context, value any) error {
	if n == nil || n.run == nil {
		return ErrResolved
	}
	converted, err := From(value)
	if err != nil {
		return err
	}
	return n.run.resolveWithValue(ctx, n, converted, ffi.ProgressResumeNameValueRaw)
}

// Undefined reports the name as undefined; Python raises a catchable
// NameError at the lookup site.
func (n *NameLookup) Undefined(ctx context.Context) error {
	if n == nil || n.run == nil {
		return ErrResolved
	}
	return n.run.resolve(ctx, n, ffi.ProgressResumeNameUndefined)
}

// Gather is a paused wait on deferred external calls (Python is awaiting
// futures created by Call.Defer or async host functions).
type Gather struct {
	ids     []CallID
	unowned []CallID

	run *Run
}

func (*Gather) isInterrupt() {}

// CallIDs returns the deferred call IDs the caller must resolve. IDs owned by
// async host functions are excluded — their results merge in automatically on
// Resolve.
func (g *Gather) CallIDs() []CallID {
	if g == nil {
		return nil
	}
	out := make([]CallID, len(g.unowned))
	copy(out, g.unowned)
	return out
}

// Resolve provides results for deferred calls and resumes execution. Results
// for async host-function calls are collected automatically and merged in.
func (g *Gather) Resolve(ctx context.Context, results ...FutureResult) error {
	if g == nil || g.run == nil {
		return ErrResolved
	}
	return g.run.resolveGather(ctx, g, results)
}

// FutureResult is the outcome of one deferred external call.
type FutureResult struct {
	id      CallID
	kind    uint32
	value   Value
	excType string
	message string
	convErr error
}

// FutureValue resolves a deferred call with a result value (converted with
// the same rules as From; conversion errors surface from Gather.Resolve).
func FutureValue(id CallID, result any) FutureResult {
	value, err := From(result)
	return FutureResult{id: id, kind: ffi.FutureResultReturn, value: value, convErr: err}
}

// FutureError resolves a deferred call by raising err inside Python.
func FutureError(id CallID, err error) FutureResult {
	excType, message := exceptionFromError(err)
	return FutureResult{id: id, kind: ffi.FutureResultError, excType: excType, message: message}
}

// FutureNotFound reports that the named external function does not exist.
func FutureNotFound(id CallID, name string) FutureResult {
	return FutureResult{id: id, kind: ffi.FutureResultNotFound, message: name}
}

// --------------------------------------------------------------------------
// Run
// --------------------------------------------------------------------------

type runState uint8

const (
	statePaused runState = iota
	stateDone
	stateFailed
	stateClosed
)

// Run is one pausable execution of a Program (or REPL snippet).
//
// While Paused, Pending reports the interrupt awaiting resolution; resuming
// through the interrupt's methods advances the Run in place. A paused Run
// serializes with Dump and restores with LoadRun (or LoadREPLRun). Runs are
// safe for concurrent use but designed to be driven by one goroutine.
type Run struct {
	mu      sync.Mutex
	state   runState
	handle  uintptr
	cleanup runtime.Cleanup
	pending Interrupt
	result  Value
	err     error

	config      runConfig
	cancelToken uintptr
	repl        *REPL
	async       *asyncDispatch

	// typeSnippet, when set on a REPL run, joins the session's accumulated
	// type-check context once the run completes successfully.
	typeSnippet string

	// finishedBeforeClose preserves Result availability after Close.
	finishedBeforeClose bool
}

// Paused reports whether an interrupt is awaiting resolution.
func (r *Run) Paused() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state == statePaused
}

// Pending returns the current interrupt. It returns the same value until that
// interrupt is resolved, and nil once the run finishes.
func (r *Run) Pending() Interrupt {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pending
}

// Result returns the final value once the run completes, the Python error if
// it failed, or ErrPaused while an interrupt is outstanding. It may be called
// repeatedly.
func (r *Run) Result() (Value, error) {
	if r == nil {
		return Value{}, ErrClosed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.state {
	case statePaused:
		return Value{}, ErrPaused
	case stateDone:
		return r.result, nil
	case stateFailed:
		return Value{}, r.err
	default:
		if r.finishedBeforeClose {
			if r.err != nil {
				return Value{}, r.err
			}
			return r.result, nil
		}
		return Value{}, ErrClosed
	}
}

// ResultAs combines Run.Result with As.
func ResultAs[T any](r *Run) (T, error) {
	var zero T
	value, err := r.Result()
	if err != nil {
		return zero, err
	}
	return As[T](value)
}

// Dump serializes the paused execution so it can be resumed later with
// LoadRun (or LoadREPLRun for REPL snippets), possibly in another process.
// Dispatch state — functions, mounts, writers — is not serialized and must be
// re-provided on load.
func (r *Run) Dump() ([]byte, error) {
	if r == nil {
		return nil, ErrClosed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.state {
	case stateClosed:
		return nil, ErrClosed
	case stateDone, stateFailed:
		return nil, ErrNotPaused
	default:
	}
	payload, err := ffi.ProgressDump(r.handle)
	if err != nil {
		return nil, normalizeError(err)
	}
	return wrapSnapshot(snapshotKindRun, payload), nil
}

// Close releases the execution. Closing a paused run abandons it; closing a
// finished run keeps its Result available. Close cancels and waits for any
// outstanding async host-function goroutines.
func (r *Run) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	switch r.state {
	case stateClosed:
		r.mu.Unlock()
		return
	case statePaused:
		r.cleanup.Stop()
		if r.handle != 0 {
			ffi.ProgressFree(r.handle)
			r.handle = 0
		}
		if r.repl != nil {
			// Closing a paused REPL run frees the session captured inside
			// the progress handle; the owning REPL can never get it back.
			r.repl.sessionLost()
		}
	case stateDone, stateFailed:
		r.finishedBeforeClose = true
	}
	r.state = stateClosed
	r.pending = nil
	r.releaseCancelTokenLocked()
	async := r.async
	r.async = nil
	r.mu.Unlock()
	if async != nil {
		async.close()
	}
}

func (r *Run) releaseCancelTokenLocked() {
	if r.cancelToken != 0 {
		ffi.CancelTokenFree(r.cancelToken)
		r.cancelToken = 0
	}
}

func (r *Run) registerCleanupLocked() {
	if r.handle != 0 {
		r.cleanup = runtime.AddCleanup(r, ffi.ProgressFree, r.handle)
	}
}

// resolveWithValue resumes the current interrupt with a Value payload.
func (r *Run) resolveWithValue(ctx context.Context, from Interrupt, value Value, resume func(uintptr, *ffi.RawValue) (ffi.StepResult, error)) error {
	if err := ffi.EnsureLoaded(); err != nil {
		return err
	}
	arena := &rawArena{}
	raw, err := valueToRaw(value, arena)
	if err != nil {
		return err
	}
	ran := false
	resolveErr := r.resolve(ctx, from, func(handle uintptr) (ffi.StepResult, error) {
		ran = true
		step, err := resume(handle, &raw)
		runtime.KeepAlive(arena)
		runtime.KeepAlive(value)
		return step, err
	})
	if !ran {
		// The FFI never consumed the payload; release any owned handles.
		freeOwnedRawValue(&raw)
	}
	return resolveErr
}

// resolve runs one resume step for the current interrupt. On a context or
// validation error the run state is unchanged (the interrupt stays pending);
// once the FFI call runs, the underlying handle is consumed and the run
// transitions to the next pause, completion, or sticky failure.
func (r *Run) resolve(ctx context.Context, from Interrupt, do func(handle uintptr) (ffi.StepResult, error)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == stateClosed {
		return ErrClosed
	}
	if r.state != statePaused || r.pending != from {
		return ErrResolved
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	handle := r.handle
	r.handle = 0
	r.cleanup.Stop()
	r.pending = nil
	stop := r.watchCancelLocked(ctx)
	step, err := do(handle)
	stop()
	if retained, ok := errors.AsType[*ffi.RetainedError](err); ok {
		// The FFI rejected the resume payload without consuming the paused
		// state: restore it so the interrupt can be resolved again.
		r.handle = handle
		r.pending = from
		r.registerCleanupLocked()
		return fmt.Errorf("monty: resume payload rejected: %w", normalizeError(retained.Err))
	}
	r.advanceLocked(ctx, step, err)
	if r.state == stateFailed {
		return r.err
	}
	return nil
}

func (r *Run) resolveGather(ctx context.Context, g *Gather, results []FutureResult) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == stateClosed {
		return ErrClosed
	}
	if r.state != statePaused || r.pending != Interrupt(g) {
		return ErrResolved
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for i := range results {
		if results[i].convErr != nil {
			return fmt.Errorf("monty: future result for call %d: %w", results[i].id, results[i].convErr)
		}
	}
	// Caller-supplied results convert strictly before any async result is
	// consumed, so a bad value leaves the run paused and fully retryable.
	merged, arena, err := futureResultsToFFI(results)
	if err != nil {
		return err
	}
	// Collect async-owned results before consuming the handle so a context
	// cancellation while waiting also leaves the run paused and resumable.
	var collected []FutureResult
	if r.async != nil {
		owned := r.async.ownedOf(g.ids)
		if len(owned) != 0 {
			collected, err = r.async.collect(ctx, owned)
			if err != nil {
				freeFutureResultValues(merged)
				return err
			}
			merged = append(merged, collectedToFFI(collected, arena)...)
		}
	}
	handle := r.handle
	r.handle = 0
	r.cleanup.Stop()
	r.pending = nil
	stop := r.watchCancelLocked(ctx)
	step, ffiErr := ffi.ProgressResumeFutures(handle, merged)
	stop()
	runtime.KeepAlive(arena)
	runtime.KeepAlive(results)
	runtime.KeepAlive(collected)
	if ffiErr != nil {
		freeFutureResultValues(merged)
	}
	if retained, ok := errors.AsType[*ffi.RetainedError](ffiErr); ok {
		// The paused state was not consumed: restore it, and re-shelve the
		// collected async results so a retried Resolve still finds them.
		r.handle = handle
		r.pending = g
		r.registerCleanupLocked()
		if r.async != nil {
			r.async.restore(collected)
		}
		return fmt.Errorf("monty: resume payload rejected: %w", normalizeError(retained.Err))
	}
	r.advanceLocked(ctx, step, ffiErr)
	if r.state == stateFailed {
		return r.err
	}
	return nil
}

func futureResultsToFFI(results []FutureResult) ([]ffi.FutureResult, *rawArena, error) {
	arena := &rawArena{}
	out := make([]ffi.FutureResult, len(results))
	for i := range results {
		result := &results[i]
		out[i] = ffi.FutureResult{
			CallID:  uint32(result.id),
			Kind:    result.kind,
			ExcType: ffi.StringRef(result.excType),
			Message: ffi.StringRef(result.message),
		}
		if result.kind == ffi.FutureResultReturn {
			raw, err := valueToRaw(result.value, arena)
			if err != nil {
				freeFutureResultValues(out[:i])
				return nil, nil, err
			}
			out[i].Value = raw
		}
	}
	return out, arena, nil
}

func freeFutureResultValues(results []ffi.FutureResult) {
	for i := range results {
		freeOwnedRawValue(&results[i].Value)
	}
}

// collectedToFFI converts async-collected results. A value that fails to
// convert degrades into the Python exception it would have raised on the sync
// host path, so one bad async result cannot poison the whole gather.
func collectedToFFI(collected []FutureResult, arena *rawArena) []ffi.FutureResult {
	out := make([]ffi.FutureResult, len(collected))
	for i := range collected {
		result := &collected[i]
		if result.kind == ffi.FutureResultReturn {
			raw, err := valueToRaw(result.value, arena)
			if err == nil {
				out[i] = ffi.FutureResult{CallID: uint32(result.id), Kind: result.kind, Value: raw}
				continue
			}
			// The substituted entry lives in collected, which the caller
			// keeps alive until the FFI call returns.
			*result = FutureError(result.id, err)
		}
		out[i] = ffi.FutureResult{
			CallID:  uint32(result.id),
			Kind:    result.kind,
			ExcType: ffi.StringRef(result.excType),
			Message: ffi.StringRef(result.message),
		}
	}
	return out
}

// watchCancelLocked arms the run's cancel token against ctx for the duration
// of one blocking FFI call. The returned stop function must be called when
// the call returns.
func (r *Run) watchCancelLocked(ctx context.Context) func() {
	if r.cancelToken == 0 || ctx == nil || ctx.Done() == nil {
		return func() {}
	}
	return watchCancel(ctx, r.cancelToken)
}

// watchCancel forwards a context cancellation to a Rust cancel token while a
// blocking FFI call runs on the calling goroutine.
func watchCancel(ctx context.Context, token uintptr) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			ffi.CancelTokenCancel(token)
		case <-stop:
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// advanceLocked is the auto-dispatch pump: it applies one FFI step outcome,
// then keeps resolving interrupts the run's configuration can answer —
// registered functions, mounts, fs filesystems, the OS handler, async future
// batches — until execution finishes or an interrupt must surface to the
// caller.
func (r *Run) advanceLocked(ctx context.Context, step ffi.StepResult, ffiErr error) {
	for {
		if writeErr := writePrint(&r.config, step.Print); writeErr != nil {
			if step.Progress != 0 {
				ffi.ProgressFree(step.Progress)
			}
			freeStepSnapshot(&step)
			r.adoptRepl(step.Repl)
			r.failLocked(ctx, writeErr)
			return
		}
		r.adoptRepl(step.Repl)
		if ffiErr != nil {
			r.failLocked(ctx, ffiErr)
			return
		}
		if step.Progress == 0 {
			value, err := decodeRawValue(step.Snapshot.Value)
			if err != nil {
				r.failLocked(ctx, err)
				return
			}
			r.result = value
			r.state = stateDone
			r.pending = nil
			r.releaseCancelTokenLocked()
			if r.repl != nil && r.typeSnippet != "" {
				r.repl.recordSnippet(r.typeSnippet)
			}
			return
		}
		interrupt, err := r.interruptFromStep(step)
		if err != nil {
			ffi.ProgressFree(step.Progress)
			r.failLocked(ctx, err)
			return
		}
		var action func(uintptr) (ffi.StepResult, error)
		// When the context is done the interrupt surfaces un-dispatched: the
		// caller observes the cancellation and the run stays resumable.
		if ctx.Err() == nil {
			action = r.autoActionLocked(ctx, interrupt)
		}
		if r.state == stateFailed {
			// autoActionLocked failed while preparing (e.g. async wait
			// cancelled): the progress handle is still live but unused.
			ffi.ProgressFree(step.Progress)
			return
		}
		if action == nil {
			r.handle = step.Progress
			r.pending = interrupt
			r.state = statePaused
			r.registerCleanupLocked()
			return
		}
		stop := r.watchCancelLocked(ctx)
		progress := step.Progress
		step, ffiErr = action(progress)
		stop()
		var retained *ffi.RetainedError
		if errors.As(ffiErr, &retained) {
			// Auto-dispatch payloads are validated before encoding, so a
			// retained rejection is exceptional: release the still-live
			// handle and fall through to the sticky failure.
			ffi.ProgressFree(progress)
		}
	}
}

// freeStepSnapshot releases the Rust-owned snapshot buffers of a step that
// will not be decoded (failure paths that bypass interruptFromStep and the
// completion decode).
func freeStepSnapshot(step *ffi.StepResult) {
	ffi.MaybeBytesFree(step.Snapshot.Name)
	step.Snapshot.Name = ffi.Bytes{}
	ffi.RawValueFree(&step.Snapshot.Args)
	ffi.RawValueFree(&step.Snapshot.Kwargs)
	ffi.RawValueFree(&step.Snapshot.Value)
}

// autoActionLocked decides how the run's configuration answers interrupt.
// It returns nil when the interrupt must surface to the caller; otherwise it
// returns the FFI resume to perform. It may mark the run failed for
// non-recoverable dispatch errors.
func (r *Run) autoActionLocked(ctx context.Context, interrupt Interrupt) func(uintptr) (ffi.StepResult, error) {
	switch req := interrupt.(type) {
	case *Call:
		if req.OS {
			if !r.config.hasOSDispatch() {
				return nil
			}
			value, err := r.config.dispatchOS(ctx, req)
			switch {
			case errors.Is(err, ErrNotHandled):
				return ffi.ProgressResumeNotHandled
			case err != nil:
				excType, message := exceptionFromError(err)
				return func(handle uintptr) (ffi.StepResult, error) {
					return ffi.ProgressResumeException(handle, excType, message)
				}
			default:
				return resumeWithValueAction(value)
			}
		}
		function := r.config.functions[req.Name]
		if function == nil {
			return nil
		}
		if function.async {
			if r.async == nil {
				r.async = newAsyncDispatch()
			}
			r.async.launch(ctx, function, req)
			return ffi.ProgressResumePending
		}
		result, err := callHostFunction(ctx, function, req.Args, req.Kwargs)
		if err != nil {
			excType, message := exceptionFromError(err)
			return func(handle uintptr) (ffi.StepResult, error) {
				return ffi.ProgressResumeException(handle, excType, message)
			}
		}
		return resumeWithValueAction(result)
	case *NameLookup:
		function := r.config.functions[req.Name]
		if function == nil {
			return nil
		}
		return resumeNameAction(functionValue(function.name, function.doc))
	case *Gather:
		if r.async == nil || len(req.unowned) != 0 {
			return nil
		}
		collected, err := r.async.collect(ctx, req.ids)
		if err != nil {
			r.failLocked(ctx, err)
			return nil
		}
		arena := &rawArena{}
		ffiResults := collectedToFFI(collected, arena)
		return func(handle uintptr) (ffi.StepResult, error) {
			step, err := ffi.ProgressResumeFutures(handle, ffiResults)
			runtime.KeepAlive(arena)
			runtime.KeepAlive(collected)
			if err != nil {
				freeFutureResultValues(ffiResults)
			}
			return step, err
		}
	default:
		return nil
	}
}

// callHostFunction invokes a sync host function, converting panics into
// Python RuntimeError instead of unwinding through the dispatch loop.
func callHostFunction(ctx context.Context, function *Function, args []Value, kwargs map[string]Value) (value Value, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("monty: host function panicked: %v", recovered)
		}
	}()
	return function.call(ctx, args, kwargs)
}

func resumeWithValueAction(value Value) func(uintptr) (ffi.StepResult, error) {
	return func(handle uintptr) (ffi.StepResult, error) {
		arena := &rawArena{}
		raw, err := valueToRaw(value, arena)
		if err != nil {
			ffi.ProgressFree(handle)
			return ffi.StepResult{}, err
		}
		step, err := ffi.ProgressResumeReturnRaw(handle, &raw)
		runtime.KeepAlive(arena)
		runtime.KeepAlive(value)
		if err != nil {
			freeOwnedRawValue(&raw)
		}
		return step, err
	}
}

func resumeNameAction(value Value) func(uintptr) (ffi.StepResult, error) {
	return func(handle uintptr) (ffi.StepResult, error) {
		arena := &rawArena{}
		raw, err := valueToRaw(value, arena)
		if err != nil {
			ffi.ProgressFree(handle)
			return ffi.StepResult{}, err
		}
		step, err := ffi.ProgressResumeNameValueRaw(handle, &raw)
		runtime.KeepAlive(arena)
		runtime.KeepAlive(value)
		if err != nil {
			freeOwnedRawValue(&raw)
		}
		return step, err
	}
}

func (r *Run) adoptRepl(handle uintptr) {
	if handle == 0 {
		return
	}
	if r.repl != nil {
		r.repl.adoptHandle(handle)
		return
	}
	ffi.ReplFree(handle)
}

// failLocked records the sticky failure, wrapping in the context error when
// the failure was caused by cancellation or a deadline-derived limit.
func (r *Run) failLocked(ctx context.Context, err error) {
	r.err = wrapCtxErrorBound(ctx, execError(err), r.config.deadlineBound)
	r.pending = nil
	r.state = stateFailed
	r.releaseCancelTokenLocked()
	if r.repl != nil {
		// Every step adopts a recovered session before reaching here, so a
		// REPL run that fails while the session is still out has lost it.
		r.repl.sessionLost()
	}
}

// ctxExecError ties a limit/cancellation ExecError to the context error that
// triggered it, so errors.Is(err, context.Canceled/DeadlineExceeded) and
// errors.As(&execErr) both hold.
type ctxExecError struct {
	exec   *ExecError
	ctxErr error
}

func (e *ctxExecError) Error() string   { return e.exec.Error() }
func (e *ctxExecError) Unwrap() []error { return []error{e.exec, e.ctxErr} }

// interruptFromStep decodes the pause snapshot into a typed interrupt.
func (r *Run) interruptFromStep(step ffi.StepResult) (Interrupt, error) {
	snapshot := step.Snapshot
	switch snapshot.Kind {
	case ffi.ProgressFunctionCall, ffi.ProgressOSCall:
		name := ffi.TakeString(snapshot.Name)
		args, err := valuesFromRawList(snapshot.Args)
		if err != nil {
			ffi.RawValueFree(&snapshot.Kwargs)
			return nil, err
		}
		kwargs, err := kwargsFromRawDict(snapshot.Kwargs)
		if err != nil {
			return nil, err
		}
		return &Call{
			Name:   name,
			Args:   args,
			Kwargs: kwargs,
			ID:     CallID(snapshot.CallID),
			OS:     snapshot.Kind == ffi.ProgressOSCall,
			Method: snapshot.MethodCall != 0,
			run:    r,
		}, nil
	case ffi.ProgressNameLookup:
		return &NameLookup{Name: ffi.TakeString(snapshot.Name), run: r}, nil
	case ffi.ProgressResolveFutures:
		ids := pendingCallIDs(step.Progress)
		gather := &Gather{ids: ids, run: r}
		if r.async != nil {
			gather.unowned = r.async.unownedOf(ids)
		} else {
			gather.unowned = ids
		}
		return gather, nil
	default:
		return nil, fmt.Errorf("monty: unknown progress kind %d", snapshot.Kind)
	}
}

func valuesFromRawList(raw ffi.RawValue) ([]Value, error) {
	value, err := decodeRawValue(raw)
	if err != nil {
		return nil, err
	}
	if value.kind != ListKind {
		return nil, fmt.Errorf("monty: progress args snapshot is %s, not list", value.kind)
	}
	return value.items, nil
}

func kwargsFromRawDict(raw ffi.RawValue) (map[string]Value, error) {
	value, err := decodeRawValue(raw)
	if err != nil {
		return nil, err
	}
	if value.kind != DictKind {
		return nil, fmt.Errorf("monty: progress kwargs snapshot is %s, not dict", value.kind)
	}
	if len(value.pairs) == 0 {
		return nil, nil
	}
	kwargs := make(map[string]Value, len(value.pairs))
	for i := range value.pairs {
		pair := &value.pairs[i]
		if pair.Key.kind != StringKind {
			return nil, fmt.Errorf("monty: keyword argument key is %s, not string", pair.Key.kind)
		}
		kwargs[pair.Key.text] = pair.Value
	}
	return kwargs, nil
}

func pendingCallIDs(handle uintptr) []CallID {
	pendingCount := int(ffi.ProgressPendingLen(handle))
	ids := make([]CallID, pendingCount)
	for i := range pendingCount {
		ids[i] = CallID(ffi.ProgressPendingID(handle, uintptr(i)))
	}
	return ids
}

// writePrint routes collected print output to the configured writers.
func writePrint(config *runConfig, printed ffi.Printed) error {
	if printed.Empty() || (config.stdout == nil && config.stderr == nil) {
		return nil
	}
	return printed.ForEach(func(stream uint8, text string) error {
		writer := config.stdout
		if stream == ffi.StreamStderr {
			writer = config.stderr
		}
		if writer == nil {
			return nil
		}
		_, err := io.WriteString(writer, text)
		return err
	})
}

// --------------------------------------------------------------------------
// Async host-function dispatch
// --------------------------------------------------------------------------

type asyncDispatch struct {
	mu      sync.Mutex
	wg      sync.WaitGroup
	cancels []context.CancelFunc
	results map[CallID]chan FutureResult
}

func newAsyncDispatch() *asyncDispatch {
	return &asyncDispatch{results: map[CallID]chan FutureResult{}}
}

// launch starts the host function on its own goroutine; the result is
// collected when Python awaits the corresponding future.
func (a *asyncDispatch) launch(ctx context.Context, function *Function, call *Call) {
	if ctx == nil {
		ctx = context.Background()
	}
	callCtx, cancel := context.WithCancel(ctx) //nolint:gosec // G118: cancel is retained in a.cancels and invoked by close()
	resultCh := make(chan FutureResult, 1)
	a.mu.Lock()
	a.results[call.ID] = resultCh
	a.cancels = append(a.cancels, cancel)
	a.mu.Unlock()
	id, args, kwargs := call.ID, call.Args, call.Kwargs
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		value, err := callHostFunction(callCtx, function, args, kwargs)
		if err != nil {
			resultCh <- FutureError(id, err)
			return
		}
		resultCh <- FutureResult{id: id, kind: ffi.FutureResultReturn, value: value}
	}()
}

func (a *asyncDispatch) owns(id CallID) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.results[id]
	return ok
}

func (a *asyncDispatch) ownedOf(ids []CallID) []CallID {
	var owned []CallID
	for _, id := range ids {
		if a.owns(id) {
			owned = append(owned, id)
		}
	}
	return owned
}

func (a *asyncDispatch) unownedOf(ids []CallID) []CallID {
	var unowned []CallID
	for _, id := range ids {
		if !a.owns(id) {
			unowned = append(unowned, id)
		}
	}
	return unowned
}

// collect waits for the results of the given owned calls.
func (a *asyncDispatch) collect(ctx context.Context, ids []CallID) ([]FutureResult, error) {
	results := make([]FutureResult, 0, len(ids))
	for _, id := range ids {
		a.mu.Lock()
		resultCh := a.results[id]
		delete(a.results, id)
		a.mu.Unlock()
		if resultCh == nil {
			return nil, fmt.Errorf("monty: no async result pending for call %d", id)
		}
		select {
		case result := <-resultCh:
			results = append(results, result)
		case <-ctx.Done():
			// Put the channel back so a later resolve can still collect it.
			a.mu.Lock()
			a.results[id] = resultCh
			a.mu.Unlock()
			return nil, ctx.Err()
		}
	}
	return results, nil
}

// restore re-shelves collected results after a resume that did not consume
// them, so a retried Resolve can collect them again.
func (a *asyncDispatch) restore(results []FutureResult) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range results {
		resultCh := make(chan FutureResult, 1)
		resultCh <- results[i]
		a.results[results[i].id] = resultCh
	}
}

func (a *asyncDispatch) close() {
	a.mu.Lock()
	cancels := a.cancels
	a.cancels = nil
	a.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	a.wg.Wait()
}

// --------------------------------------------------------------------------
// Snapshot envelope and loading
// --------------------------------------------------------------------------

const (
	snapshotKindRun  = 0x01
	snapshotKindREPL = 0x02
)

var snapshotMagic = []byte{'M', 'G', 'O', '1'}

func wrapSnapshot(kind byte, payload []byte) []byte {
	out := make([]byte, 0, len(snapshotMagic)+1+len(payload))
	out = append(out, snapshotMagic...)
	out = append(out, kind)
	return append(out, payload...)
}

func unwrapSnapshot(data []byte, wantKind byte) ([]byte, error) {
	if len(data) <= len(snapshotMagic) || !bytes.Equal(data[:4], snapshotMagic) {
		return nil, fmt.Errorf("monty: not a monty-go snapshot")
	}
	kind := data[4]
	if kind != wantKind {
		return nil, fmt.Errorf("monty: snapshot kind %d does not match the loader (run dumps load with LoadRun/LoadREPLRun, REPL sessions with LoadREPL)", kind)
	}
	return data[5:], nil
}

// LoadRun restores a paused Program execution serialized by Run.Dump.
// Dispatch options — functions, mounts, fs filesystems, writers, limits — are
// not part of the snapshot and must be re-provided here.
func LoadRun(data []byte, opts ...RunOption) (*Run, error) {
	payload, err := unwrapSnapshot(data, snapshotKindRun)
	if err != nil {
		return nil, err
	}
	handle, err := ffi.ProgressLoad(payload)
	if err != nil {
		return nil, normalizeError(err)
	}
	if ffi.ProgressIsRepl(handle) {
		ffi.ProgressFree(handle)
		return nil, fmt.Errorf("monty: snapshot is a REPL execution; load it with LoadREPLRun")
	}
	return runFromLoadedHandle(handle, nil, opts...)
}

// runFromLoadedHandle builds a paused Run around a freshly loaded progress
// handle.
func runFromLoadedHandle(handle uintptr, repl *REPL, opts ...RunOption) (*Run, error) {
	var config runConfig
	for _, opt := range opts {
		opt.applyRun(&config)
	}
	if config.skipTypeCheck {
		ffi.ProgressFree(handle)
		return nil, fmt.Errorf("monty: WithoutTypeCheck does not apply to a loaded run")
	}
	snapshot, err := ffi.ProgressSnapshotGet(handle)
	if err != nil {
		ffi.ProgressFree(handle)
		return nil, normalizeError(err)
	}
	r := &Run{state: statePaused, config: config, repl: repl}
	step := ffi.StepResult{Progress: handle, Snapshot: snapshot}
	// A cancel token attaches best-effort (only tracked function-call states
	// expose their tracker after a load).
	if token, tokenErr := ffi.CancelTokenNew(); tokenErr == nil {
		if ffi.ProgressSetCancelToken(handle, token) {
			r.cancelToken = token
		} else {
			ffi.CancelTokenFree(token)
		}
	}
	r.mu.Lock()
	r.advanceLocked(context.Background(), step, nil)
	state := r.state
	err = r.err
	r.mu.Unlock()
	if state == stateFailed {
		r.Close()
		return nil, err
	}
	return r, nil
}
