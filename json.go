package monty

import (
	"context"
	"fmt"
	"runtime"

	"github.com/joeychilson/monty/internal/ffi"
)

// MarshalJSON returns Monty's natural JSON form for v.
//
// It intentionally mirrors upstream JsonMontyObject semantics: JSON-native
// values serialize directly, while Python-only values use tagged objects such
// as {"$tuple": [...]} or {"$bytes": [...]}.
func (v Value) MarshalJSON() ([]byte, error) {
	handle, err := valueHandle(v)
	if err != nil {
		return nil, err
	}
	defer ffi.ValueFree(handle)
	json, err := ffi.ValueJSON(handle)
	return json, normalizeError(err)
}

// JSON returns Monty's natural JSON form for v.
func (v Value) JSON() ([]byte, error) {
	return v.MarshalJSON()
}

// JSONString returns Monty's natural JSON form for v as a string.
func (v Value) JSONString() (string, error) {
	json, err := v.JSON()
	if err != nil {
		return "", err
	}
	return string(json), nil
}

// MarshalJSON returns the completed value in Monty's natural JSON form.
func (complete *Complete) MarshalJSON() ([]byte, error) {
	if complete == nil {
		return None().JSON()
	}
	return complete.Value.JSON()
}

// JSON returns the completed value in Monty's natural JSON form.
func (complete *Complete) JSON() ([]byte, error) {
	return complete.MarshalJSON()
}

// JSONString returns the completed value in Monty's natural JSON form as a string.
func (complete *Complete) JSONString() (string, error) {
	json, err := complete.JSON()
	if err != nil {
		return "", err
	}
	return string(json), nil
}

// RunJSON executes program and returns the final value in Monty's natural JSON form.
func RunJSON(ctx context.Context, program *Program, inputs any, opts ...RunOption) ([]byte, error) {
	if program == nil {
		return nil, fmt.Errorf("monty: program is closed")
	}
	return program.RunJSON(ctx, inputs, opts...)
}

// RunJSON executes p and returns the final value in Monty's natural JSON form.
func (p *Program) RunJSON(ctx context.Context, inputs any, opts ...RunOption) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("monty: program is closed")
	}
	var config runConfig
	if len(opts) == 0 {
		config.functions = p.functions
	} else {
		config = p.runConfig(opts...)
	}
	if !config.needsDispatchLoop() {
		return p.runJSONDirect(ctx, inputs, config)
	}
	value, err := p.Run(ctx, inputs, opts...)
	if err != nil {
		return nil, err
	}
	return value.JSON()
}

func (p *Program) runJSONDirect(ctx context.Context, inputs any, config runConfig) ([]byte, error) {
	config, err := p.prepareRun(ctx, config)
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
	var json []byte
	var printed string
	p.mu.RLock()
	if p.handle == 0 {
		p.mu.RUnlock()
		p.releaseRawInputs(rawInputs, keepAlive)
		runtime.KeepAlive(keepAlive)
		return nil, fmt.Errorf("monty: program is closed")
	}
	json, printed, err = ffi.ProgramRunJSONRaw(p.handle, rawInputs, ffiLimits)
	p.mu.RUnlock()
	p.releaseRawInputs(rawInputs, keepAlive)
	runtime.KeepAlive(keepAlive)
	writeErr := writePrinted(config.stdout, printed)
	if err != nil {
		return nil, joinErrors(normalizeError(err), writeErr)
	}
	if writeErr != nil {
		return nil, writeErr
	}
	return json, nil
}

// ArgsJSON returns a paused function call's positional args in Monty's natural JSON form.
func (call *FunctionCall) ArgsJSON() ([]byte, error) {
	if call == nil {
		return List().JSON()
	}
	return Value{kind: ListKind, items: call.Args}.JSON()
}

// KwargsJSON returns a paused function call's keyword args in Monty's natural JSON form.
func (call *FunctionCall) KwargsJSON() ([]byte, error) {
	if call == nil {
		return Dict().JSON()
	}
	return Value{kind: DictKind, pairs: call.Kwargs}.JSON()
}

// ArgsJSON returns a paused OS call's positional args in Monty's natural JSON form.
func (call *OSCall) ArgsJSON() ([]byte, error) {
	if call == nil {
		return List().JSON()
	}
	return Value{kind: ListKind, items: call.Args}.JSON()
}

// KwargsJSON returns a paused OS call's keyword args in Monty's natural JSON form.
func (call *OSCall) KwargsJSON() ([]byte, error) {
	if call == nil {
		return Dict().JSON()
	}
	return Value{kind: DictKind, pairs: call.Kwargs}.JSON()
}
