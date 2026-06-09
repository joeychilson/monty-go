package monty

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"sync"

	"github.com/joeychilson/monty/internal/ffi"
)

// Progress is an execution state returned by Program.Start or a Resume method.
//
// Progress values are single-use handles when they represent paused execution.
// Call Close if a paused state will not be resumed.
type Progress interface {
	// Close releases any Rust-side state held by this progress value.
	Close() error
	// Dump serializes a paused progress value for later LoadProgress.
	Dump() ([]byte, error)
}

type progressBase struct {
	handle  uintptr
	stdout  io.Writer
	mu      sync.Mutex
	cleanup runtime.Cleanup
}

// registerCleanup attaches a cleanup that frees the progress handle if the value
// is dropped without Close or a resume. The cleanup captures the handle value;
// both Close and take stop it before releasing or consuming the handle, so it is
// freed exactly once.
func (p *progressBase) registerCleanup() {
	if p.handle != 0 {
		p.cleanup = runtime.AddCleanup(p, ffi.ProgressFree, p.handle)
	}
}

func (p *progressBase) Close() error {
	if p == nil {
		return nil
	}
	p.cleanup.Stop()
	p.mu.Lock()
	handle := p.handle
	p.handle = 0
	p.mu.Unlock()
	if handle != 0 {
		ffi.ProgressFree(handle)
	}
	return nil
}

func (p *progressBase) Dump() ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("monty: progress is closed")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.handle == 0 {
		return nil, fmt.Errorf("monty: progress is closed")
	}
	snapshot, err := ffi.ProgressDump(p.handle)
	return snapshot, normalizeError(err)
}

func (p *progressBase) take() (uintptr, error) {
	if p == nil {
		return 0, fmt.Errorf("monty: progress is closed")
	}
	// The resume call about to receive this handle consumes it (Rust frees it),
	// so stop the cleanup to avoid a double free.
	p.cleanup.Stop()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.handle == 0 {
		return 0, fmt.Errorf("monty: progress is closed")
	}
	handle := p.handle
	p.handle = 0
	return handle, nil
}

// Complete is the final progress state for a finished program.
type Complete struct {
	// Value is the Python value produced by the program.
	Value Value
}

// Close is a no-op for completed progress.
func (*Complete) Close() error { return nil }

// Dump always returns an error because complete states contain no resumable execution handle.
func (*Complete) Dump() ([]byte, error) {
	return nil, fmt.Errorf("monty: complete progress cannot be dumped")
}

// FunctionCall is a paused call to a Python function resolved by Go.
type FunctionCall struct {
	progressBase
	// Name is the Python function name.
	Name string
	// Args are positional Python arguments.
	Args []Value
	// Kwargs are keyword Python arguments.
	Kwargs []Pair
	// CallID identifies the call when resolving async futures.
	CallID uint32
	// MethodCall reports whether Python called the function as a method.
	MethodCall bool
}

// Resume returns value to Python and continues execution.
func (call *FunctionCall) Resume(ctx context.Context, value Value) (Progress, error) {
	return resumeReturnSnapshot(ctx, &call.progressBase, value)
}

// ResumeException raises an exception at the paused Python call and continues execution.
func (call *FunctionCall) ResumeException(ctx context.Context, excType, message string) (Progress, error) {
	return resumeWithException(ctx, &call.progressBase, excType, message)
}

// ResumePending marks the call as pending so it can later be resolved through ResolveFutures.
func (call *FunctionCall) ResumePending(ctx context.Context) (Progress, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	handle, err := call.take()
	if err != nil {
		return nil, err
	}
	nextHandle, printed, err := ffi.ProgressResumePending(handle)
	return decodeResumeResult(call.stdout, nextHandle, printed, err)
}

// NameLookup is a paused lookup for an unknown Python global name.
type NameLookup struct {
	progressBase
	// Name is the Python global name being resolved.
	Name string
}

// Resume provides the Python value for the requested name and continues execution.
func (lookup *NameLookup) Resume(ctx context.Context, value Value) (Progress, error) {
	return resumeWithValue(ctx, &lookup.progressBase, value, ffi.ProgressResumeNameValueRaw)
}

// ResumeUndefined reports the requested name as undefined and continues execution.
func (lookup *NameLookup) ResumeUndefined(ctx context.Context) (Progress, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	handle, err := lookup.take()
	if err != nil {
		return nil, err
	}
	nextHandle, printed, err := ffi.ProgressResumeNameUndefined(handle)
	return decodeResumeResult(lookup.stdout, nextHandle, printed, err)
}

// OSCall is a paused operation that would access the host operating system.
type OSCall struct {
	progressBase
	// Function is the Monty OS function name, such as "Path.read_text".
	Function string
	// Args are positional Python arguments.
	Args []Value
	// Kwargs are keyword Python arguments.
	Kwargs []Pair
	// CallID identifies the call when resolving async futures.
	CallID uint32
}

// Resume returns value to Python and continues execution.
func (call *OSCall) Resume(ctx context.Context, value Value) (Progress, error) {
	return resumeReturnSnapshot(ctx, &call.progressBase, value)
}

// ResumeException raises an exception at the paused OS call and continues execution.
func (call *OSCall) ResumeException(ctx context.Context, excType, message string) (Progress, error) {
	return resumeWithException(ctx, &call.progressBase, excType, message)
}

// ResolveFutures is a paused state waiting for one or more pending function calls.
type ResolveFutures struct {
	progressBase
	pendingCallIDs []uint32
}

// PendingCallIDs returns the call IDs that must be resolved before execution can continue.
func (futures *ResolveFutures) PendingCallIDs() []uint32 {
	return slices.Clone(futures.pendingCallIDs)
}

// Resume resolves pending async calls and continues execution.
func (futures *ResolveFutures) Resume(ctx context.Context, results ...FutureResult) (Progress, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	handle, err := futures.take()
	if err != nil {
		return nil, err
	}
	arena := &rawArena{}
	ffiResults := make([]ffi.FutureResult, len(results))
	for i := range results {
		result := &results[i]
		ffiResults[i] = ffi.FutureResult{
			CallID:  result.CallID,
			Kind:    uint32(result.kind),
			ExcType: ffi.StringRef(result.excType),
			Message: ffi.StringRef(result.message),
		}
		if result.kind == futureResultReturn {
			raw, err := valueToRaw(result.value, arena)
			if err != nil {
				freeFutureResultValues(ffiResults)
				ffi.ProgressFree(handle)
				return nil, err
			}
			ffiResults[i].Value = raw
		}
	}
	defer freeFutureResultValues(ffiResults)
	nextHandle, printed, err := ffi.ProgressResumeFutures(handle, ffiResults)
	runtime.KeepAlive(arena)
	runtime.KeepAlive(results)
	return decodeResumeResult(futures.stdout, nextHandle, printed, err)
}

type futureResultKind uint32

const (
	futureResultReturn futureResultKind = iota
	futureResultError
	futureResultNotFound
)

// FutureResult is the outcome for a pending async call.
type FutureResult struct {
	// CallID is the pending call ID to resolve.
	CallID  uint32
	kind    futureResultKind
	value   Value
	excType string
	message string
}

// FutureValue resolves a pending call with a return value.
func FutureValue(callID uint32, value Value) FutureResult {
	return FutureResult{CallID: callID, kind: futureResultReturn, value: value}
}

// FutureException resolves a pending call by raising an exception.
func FutureException(callID uint32, excType, message string) FutureResult {
	return FutureResult{CallID: callID, kind: futureResultError, excType: excType, message: message}
}

// FutureNotFound reports that a pending external function name could not be resolved.
func FutureNotFound(callID uint32, name string) FutureResult {
	return FutureResult{CallID: callID, kind: futureResultNotFound, message: name}
}

// resumeWithValue resumes execution by passing value back through
// the supplied FFI entry point (return-value or name-lookup paths).
func resumeWithValue(
	ctx context.Context,
	base *progressBase,
	value Value,
	resume func(progress uintptr, value *ffi.RawValue) (uintptr, string, error),
) (Progress, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	handle, err := base.take()
	if err != nil {
		return nil, err
	}
	arena := &rawArena{}
	raw, err := valueToRaw(value, arena)
	if err != nil {
		ffi.ProgressFree(handle)
		return nil, err
	}
	defer freeOwnedRawValue(&raw)
	nextHandle, printed, err := resume(handle, &raw)
	runtime.KeepAlive(arena)
	runtime.KeepAlive(value)
	return decodeResumeResult(base.stdout, nextHandle, printed, err)
}

// resumeReturnSnapshot is the single-hop counterpart of resumeWithValue for the
// return-value path: it resumes and fetches the next snapshot in one FFI call,
// avoiding the separate snapshot hop progressFromHandle would make.
func resumeReturnSnapshot(ctx context.Context, base *progressBase, value Value) (Progress, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	handle, err := base.take()
	if err != nil {
		return nil, err
	}
	arena := &rawArena{}
	raw, err := valueToRaw(value, arena)
	if err != nil {
		ffi.ProgressFree(handle)
		return nil, err
	}
	defer freeOwnedRawValue(&raw)
	nextHandle, snapshot, printed, err := ffi.ProgressResumeReturnRawSnapshot(handle, &raw)
	runtime.KeepAlive(arena)
	runtime.KeepAlive(value)
	if err != nil {
		writeErr := writePrinted(base.stdout, printed)
		return nil, errors.Join(normalizeError(err), writeErr)
	}
	progress, err := progressFromSnapshot(nextHandle, snapshot, base.stdout)
	if err != nil {
		return nil, err
	}
	if writeErr := writePrinted(base.stdout, printed); writeErr != nil {
		_ = progress.Close()
		return nil, writeErr
	}
	return progress, nil
}

func freeFutureResultValues(results []ffi.FutureResult) {
	for i := range results {
		freeOwnedRawValue(&results[i].Value)
	}
}

func resumeWithException(ctx context.Context, base *progressBase, excType, message string) (Progress, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	handle, err := base.take()
	if err != nil {
		return nil, err
	}
	nextHandle, printed, err := ffi.ProgressResumeException(handle, excType, message)
	return decodeResumeResult(base.stdout, nextHandle, printed, err)
}

func decodeResumeResult(stdout io.Writer, nextHandle uintptr, printed string, err error) (Progress, error) {
	writeErr := writePrinted(stdout, printed)
	if err != nil {
		return nil, errors.Join(normalizeError(err), writeErr)
	}
	if writeErr != nil {
		ffi.ProgressFree(nextHandle)
		return nil, writeErr
	}
	return progressFromHandle(nextHandle, stdout)
}

// LoadProgress restores a paused Progress value created by Progress.Dump.
func LoadProgress(snapshot []byte) (Progress, error) {
	return LoadProgressWithStdout(snapshot, nil)
}

// LoadProgressWithStdout restores a paused Progress and routes future print output to stdout.
func LoadProgressWithStdout(snapshot []byte, stdout io.Writer) (Progress, error) {
	handle, err := ffi.ProgressLoad(snapshot)
	if err != nil {
		return nil, normalizeError(err)
	}
	return progressFromHandle(handle, stdout)
}

// progressFromHandle fetches a snapshot for handle (a second FFI hop) and builds
// the corresponding Progress. The single-hop start/resume variants call
// progressFromSnapshot directly with the snapshot they already obtained.
func progressFromHandle(handle uintptr, stdout io.Writer) (Progress, error) {
	if handle == 0 {
		return nil, fmt.Errorf("monty: null progress handle")
	}
	snapshot, err := ffi.ProgressSnapshotGet(handle)
	if err != nil {
		ffi.ProgressFree(handle)
		return nil, normalizeError(err)
	}
	return progressFromSnapshot(handle, snapshot, stdout)
}

// progressFromSnapshot builds a Progress from an already-obtained snapshot and
// its handle. handle is 0 for a Complete snapshot from the single-hop variants
// (Rust already released it); for the two-hop path it is the live handle, which
// is freed here for Complete. On any error it frees handle.
func progressFromSnapshot(handle uintptr, snapshot ffi.ProgressSnapshot, stdout io.Writer) (Progress, error) {
	switch snapshot.Kind {
	case ffi.ProgressComplete:
		if handle != 0 {
			ffi.ProgressFree(handle)
		}
		value, err := decodeRawValue(snapshot.Value)
		if err != nil {
			return nil, err
		}
		return &Complete{Value: value}, nil
	case ffi.ProgressFunctionCall:
		progress, err := functionCallFromSnapshot(handle, snapshot, stdout)
		if err != nil {
			ffi.ProgressFree(handle)
			return nil, err
		}
		progress.registerCleanup()
		return progress, nil
	case ffi.ProgressNameLookup:
		name := ffi.TakeString(snapshot.Name)
		progress := &NameLookup{
			progressBase: progressBase{handle: handle, stdout: stdout},
			Name:         name,
		}
		progress.registerCleanup()
		return progress, nil
	case ffi.ProgressOSCall:
		progress, err := osCallFromSnapshot(handle, snapshot, stdout)
		if err != nil {
			ffi.ProgressFree(handle)
			return nil, err
		}
		progress.registerCleanup()
		return progress, nil
	case ffi.ProgressResolveFutures:
		progress := &ResolveFutures{
			progressBase:   progressBase{handle: handle, stdout: stdout},
			pendingCallIDs: pendingCallIDs(handle),
		}
		progress.registerCleanup()
		return progress, nil
	default:
		ffi.ProgressFree(handle)
		return nil, fmt.Errorf("monty: unknown progress kind")
	}
}

func callFromSnapshot(snapshot ffi.ProgressSnapshot) (string, []Value, []Pair, error) {
	name := ffi.TakeString(snapshot.Name)
	args, err := valuesFromRawList(snapshot.Args)
	if err != nil {
		ffi.RawValueFree(&snapshot.Kwargs)
		return "", nil, nil, err
	}
	kwargs, err := pairsFromRawDict(snapshot.Kwargs)
	if err != nil {
		return "", nil, nil, err
	}
	return name, args, kwargs, nil
}

func functionCallFromSnapshot(handle uintptr, snapshot ffi.ProgressSnapshot, stdout io.Writer) (*FunctionCall, error) {
	name, args, kwargs, err := callFromSnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	return &FunctionCall{
		progressBase: progressBase{handle: handle, stdout: stdout},
		Name:         name,
		Args:         args,
		Kwargs:       kwargs,
		CallID:       snapshot.CallID,
		MethodCall:   snapshot.MethodCall != 0,
	}, nil
}

func osCallFromSnapshot(handle uintptr, snapshot ffi.ProgressSnapshot, stdout io.Writer) (*OSCall, error) {
	function, args, kwargs, err := callFromSnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	return &OSCall{
		progressBase: progressBase{handle: handle, stdout: stdout},
		Function:     function,
		Args:         args,
		Kwargs:       kwargs,
		CallID:       snapshot.CallID,
	}, nil
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

func pairsFromRawDict(raw ffi.RawValue) ([]Pair, error) {
	value, err := decodeRawValue(raw)
	if err != nil {
		return nil, err
	}
	if value.kind != DictKind {
		return nil, fmt.Errorf("monty: progress kwargs snapshot is %s, not dict", value.kind)
	}
	return value.pairs, nil
}

func pendingCallIDs(handle uintptr) []uint32 {
	pendingCount := int(ffi.ProgressPendingLen(handle))
	ids := make([]uint32, pendingCount)
	for i := range pendingCount {
		ids[i] = ffi.ProgressPendingID(handle, uintptr(i))
	}
	return ids
}

func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
