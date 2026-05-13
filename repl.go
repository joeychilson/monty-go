package monty

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"

	"github.com/joeychilson/monty/internal/ffi"
)

// Repl is a stateful Monty Python REPL session.
//
// A Repl preserves globals, heap state, and functions between snippets. It is
// safe to call from multiple goroutines; calls are serialized because each
// snippet mutates the session.
type Repl struct {
	mu     sync.Mutex
	handle uintptr
}

// ReplOption configures NewRepl.
type ReplOption func(*replConfig)

type replConfig struct {
	scriptName string
	limits     *Limits
}

// WithReplScriptName sets the filename used in REPL tracebacks.
func WithReplScriptName(name string) ReplOption {
	return func(c *replConfig) { c.scriptName = name }
}

// WithReplLimits applies resource limits to the REPL session.
func WithReplLimits(limits Limits) ReplOption {
	return func(c *replConfig) { c.limits = new(limits) }
}

// NewRepl creates an empty stateful REPL session.
func NewRepl(opts ...ReplOption) (*Repl, error) {
	config := replConfig{scriptName: "<repl>"}
	for _, opt := range opts {
		opt(&config)
	}
	var ffiLimits *ffi.Limits
	if config.limits != nil {
		ffiLimits = config.limits.ffi()
	}
	handle, err := ffi.ReplNew(config.scriptName, ffiLimits)
	if err != nil {
		return nil, normalizeError(err)
	}
	return &Repl{handle: handle}, nil
}

// LoadRepl restores a REPL session created by Repl.Dump.
func LoadRepl(snapshot []byte) (*Repl, error) {
	handle, err := ffi.ReplLoad(snapshot)
	if err != nil {
		return nil, normalizeError(err)
	}
	return &Repl{handle: handle}, nil
}

// Close releases the Rust-side REPL handle.
func (r *Repl) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	handle := r.handle
	r.handle = 0
	r.mu.Unlock()
	ffi.ReplFree(handle)
	return nil
}

// Dump serializes the REPL session so it can be restored with LoadRepl.
func (r *Repl) Dump() ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("monty: REPL is closed")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handle == 0 {
		return nil, fmt.Errorf("monty: REPL is closed")
	}
	snapshot, err := ffi.ReplDump(r.handle)
	return snapshot, normalizeError(err)
}

// FeedRun executes one REPL snippet to completion.
func (r *Repl) FeedRun(ctx context.Context, code string, inputs any, opts ...RunOption) (Value, error) {
	if err := ctxErr(ctx); err != nil {
		return Value{}, err
	}
	config := runConfig{}
	for _, opt := range opts {
		opt(&config)
	}
	names, handles, err := replInputHandles(inputs)
	if err != nil {
		return Value{}, err
	}
	defer freeHandles(handles)

	if r == nil {
		return Value{}, fmt.Errorf("monty: REPL is closed")
	}
	r.mu.Lock()
	if r.handle == 0 {
		r.mu.Unlock()
		return Value{}, fmt.Errorf("monty: REPL is closed")
	}
	valueHandle, printed, err := ffi.ReplFeedRun(r.handle, code, names, handles)
	r.mu.Unlock()
	return decodeReplResult(valueHandle, printed, err, config.stdout)
}

// Call is a convenience wrapper around CallFunction.
func (r *Repl) Call(ctx context.Context, name string, args ...Value) (Value, error) {
	return r.CallFunction(ctx, name, args)
}

// CallFunction calls a Python function defined in the REPL.
func (r *Repl) CallFunction(ctx context.Context, name string, args []Value, opts ...RunOption) (Value, error) {
	if err := ctxErr(ctx); err != nil {
		return Value{}, err
	}
	config := runConfig{}
	for _, opt := range opts {
		opt(&config)
	}
	handles, err := valuesToHandles(args)
	if err != nil {
		return Value{}, err
	}
	defer freeHandles(handles)

	if r == nil {
		return Value{}, fmt.Errorf("monty: REPL is closed")
	}
	r.mu.Lock()
	if r.handle == 0 {
		r.mu.Unlock()
		return Value{}, fmt.Errorf("monty: REPL is closed")
	}
	valueHandle, printed, err := ffi.ReplCallFunction(r.handle, name, handles)
	r.mu.Unlock()
	return decodeReplResult(valueHandle, printed, err, config.stdout)
}

// decodeReplResult finishes a REPL FFI call: flush print output, then either
// return the FFI error joined with any write error, or decode the value
// handle. valueHandle is freed if writing print output fails.
func decodeReplResult(valueHandle uintptr, printed string, callErr error, stdout io.Writer) (Value, error) {
	writeErr := writePrinted(stdout, printed)
	if callErr != nil {
		return Value{}, errors.Join(normalizeError(callErr), writeErr)
	}
	if writeErr != nil {
		ffi.ValueFree(valueHandle)
		return Value{}, writeErr
	}
	value, err := decodeOwnedValue(valueHandle)
	return value, normalizeError(err)
}

// FunctionNames returns Python functions currently defined in the REPL.
func (r *Repl) FunctionNames() ([]string, error) {
	if r == nil {
		return nil, fmt.Errorf("monty: REPL is closed")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handle == 0 {
		return nil, fmt.Errorf("monty: REPL is closed")
	}
	names, err := ffi.ReplFunctionNames(r.handle)
	return names, normalizeError(err)
}

// HasFunction reports whether a Python function is currently defined in the REPL.
func (r *Repl) HasFunction(name string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handle == 0 {
		return false
	}
	return ffi.ReplHasFunction(r.handle, name)
}

func replInputHandles(inputs any) ([]string, []uintptr, error) {
	inputValues, err := normalizeInputs(inputs)
	if err != nil {
		return nil, nil, err
	}
	names := slices.Sorted(maps.Keys(inputValues))
	handles := make([]uintptr, len(names))
	for i, name := range names {
		handle, err := valueToHandle(inputValues[name])
		if err != nil {
			freeHandles(handles)
			return nil, nil, err
		}
		handles[i] = handle
	}
	return names, handles, nil
}

// ReplContinuationMode describes whether interactive source is ready to run.
type ReplContinuationMode uint32

const (
	// ReplComplete means the source is syntactically complete.
	ReplComplete ReplContinuationMode = iota
	// ReplIncompleteImplicit means more input is needed for an open expression.
	ReplIncompleteImplicit
	// ReplIncompleteBlock means an indented block needs a trailing blank line.
	ReplIncompleteBlock
)

// String returns a stable display name for mode.
func (mode ReplContinuationMode) String() string {
	switch mode {
	case ReplComplete:
		return "complete"
	case ReplIncompleteImplicit:
		return "incomplete-implicit"
	case ReplIncompleteBlock:
		return "incomplete-block"
	default:
		return fmt.Sprintf("ReplContinuationMode(%d)", uint32(mode))
	}
}

// DetectReplContinuationMode reports whether interactive source is ready to run.
func DetectReplContinuationMode(code string) ReplContinuationMode {
	return ReplContinuationMode(ffi.ReplContinuationMode(code))
}

var _ io.Closer = (*Repl)(nil)
