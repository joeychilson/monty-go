package monty

import (
	"context"
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"time"
	"unsafe"

	"github.com/joeychilson/monty/internal/ffi"
)

// Function exposes a Go function to Monty Python code.
//
// Register functions with WithFunctions on Compile, NewREPL, or a run. When
// Python calls one, positional and keyword arguments bind to the handler's
// parameters and its result converts back to a Python value; a non-nil error
// raises into Python (typed when the error is a *Exception).
type Function struct {
	name        string
	doc         string
	stub        string
	async       bool
	rawHandler  RawFunc
	handler     reflect.Value
	handlerType reflect.Type
	fastRawCall func(context.Context, ffi.RawValue, ffi.RawValue) (ffi.RawValue, error)

	takesContext     bool
	returnsErrorOnly bool
	outputType       reflect.Type
	outputFields     []taggedField

	// Input binding: either a single struct parameter bound by field name
	// (kwargs style), or N positional parameters.
	structInput      bool
	inputType        reflect.Type
	inputFields      []taggedField
	inputNameToField map[string]taggedField
	params           []reflect.Type
	paramNames       []string
}

// FunctionOption configures a Function.
type FunctionOption func(*Function)

// WithDoc sets the Python doc comment used in the generated stub.
func WithDoc(text string) FunctionOption {
	return func(f *Function) { f.doc = text }
}

// WithParams names the handler's positional parameters, enabling keyword
// arguments from Python and better generated stubs. The count must match the
// handler's parameter count (excluding context.Context).
func WithParams(names ...string) FunctionOption {
	return func(f *Function) { f.paramNames = names }
}

// WithAsync marks the function as asynchronous: Python sees an `async def`
// stub, and during Program.Run each call is dispatched on its own goroutine
// and awaited where Python awaits it — so asyncio.gather branches run
// concurrently in Go.
func WithAsync() FunctionOption {
	return func(f *Function) { f.async = true }
}

// WithStub replaces the generated Python type stub verbatim.
func WithStub(stub string) FunctionOption {
	return func(f *Function) { f.stub = stub }
}

// RawFunc is the dynamic host-function form: it receives the Python call as
// Values and returns the result as a Value.
type RawFunc func(ctx context.Context, args []Value, kwargs map[string]Value) (Value, error)

// NewFunction creates a named Go host function callable from Monty Python
// code.
//
// handler must be a func. It may take context.Context as its first
// parameter. The remaining parameters bind Python arguments: a single struct
// parameter binds positional and keyword arguments to its fields (`monty`
// tags, snake_case defaults), while plain parameters bind positionally (use
// WithParams to accept keyword arguments). The handler may return up to two
// values; the second, when present, must be an error. A handler returning
// only an error yields None on success.
func NewFunction(name string, handler any, opts ...FunctionOption) (*Function, error) {
	f := &Function{name: name, handler: reflect.ValueOf(handler)}
	for _, opt := range opts {
		opt(f)
	}
	if err := f.validateHandler(); err != nil {
		return nil, err
	}
	if err := f.inspect(); err != nil {
		return nil, err
	}
	f.fastRawCall = f.fastReflectRawCall()
	return f, nil
}

// MustFunction is NewFunction for package-level registration: it panics on an
// invalid handler signature.
func MustFunction(name string, handler any, opts ...FunctionOption) *Function {
	f, err := NewFunction(name, handler, opts...)
	if err != nil {
		panic(err)
	}
	return f
}

// NewRawFunction creates a host function from the dynamic RawFunc form. Use
// it when the Python signature is not known statically; generated stubs fall
// back to *args/**kwargs unless WithStub overrides them.
func NewRawFunction(name string, handler RawFunc, opts ...FunctionOption) *Function {
	f := &Function{name: name, rawHandler: handler}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// validateHandler enforces the handler contract relied on by call and
// fastReflectRawCall: a func returning at most two values, the second of
// which (when present) implements error.
func (f *Function) validateHandler() error {
	if !f.handler.IsValid() {
		return fmt.Errorf("monty: NewFunction %q: handler must be a func, got nil", f.name)
	}
	if f.handler.Kind() != reflect.Func {
		return fmt.Errorf("monty: NewFunction %q: handler must be a func, got %s", f.name, f.handler.Type())
	}
	handlerType := f.handler.Type()
	if handlerType.IsVariadic() {
		return fmt.Errorf("monty: NewFunction %q: variadic handlers are not supported", f.name)
	}
	if handlerType.NumOut() > 2 {
		return fmt.Errorf("monty: NewFunction %q: handler must have at most 2 return values, got %d", f.name, handlerType.NumOut())
	}
	if handlerType.NumOut() == 2 && !handlerType.Out(1).Implements(reflect.TypeFor[error]()) {
		return fmt.Errorf("monty: NewFunction %q: second return value must be error, got %s", f.name, handlerType.Out(1))
	}
	return nil
}

// Name returns the Python-visible function name.
func (f *Function) Name() string { return f.name }

// Async reports whether the function was registered with WithAsync.
func (f *Function) Async() bool { return f.async }

func (f *Function) inspect() error {
	functionType := f.handler.Type()
	f.handlerType = functionType
	inputIndex := 0
	if functionType.NumIn() > 0 && functionType.In(0) == reflect.TypeFor[context.Context]() {
		f.takesContext = true
		inputIndex = 1
	}
	remaining := functionType.NumIn() - inputIndex

	switch {
	case remaining == 0:
		// No Python-visible parameters.
	case remaining == 1 && isStructContainer(functionType.In(inputIndex)):
		f.structInput = true
		f.inputType = functionType.In(inputIndex)
		inputInfo := taggedFieldsFor(f.inputType)
		f.inputFields = inputInfo.fields
		f.inputNameToField = inputInfo.nameToField
	default:
		f.params = make([]reflect.Type, remaining)
		for i := range remaining {
			f.params[i] = functionType.In(inputIndex + i)
		}
	}
	if len(f.paramNames) != 0 {
		if f.structInput {
			return fmt.Errorf("monty: NewFunction %q: WithParams does not apply to a struct parameter", f.name)
		}
		if len(f.paramNames) != len(f.params) {
			return fmt.Errorf("monty: NewFunction %q: WithParams names %d parameters, handler has %d", f.name, len(f.paramNames), len(f.params))
		}
	}

	// A handler whose sole return value is an error reports failure, not data:
	// a non-nil error must be raised as a Python exception and a nil error must
	// surface as None. Leaving outputType nil keeps it out of the value path
	// (call returns None on success) and makes PythonStub emit `-> None`.
	if functionType.NumOut() == 1 && functionType.Out(0).Implements(reflect.TypeFor[error]()) {
		f.returnsErrorOnly = true
	} else if functionType.NumOut() > 0 {
		f.outputType = functionType.Out(0)
	}
	if f.outputType != nil {
		f.outputFields = taggedFieldsFor(f.outputType).fields
	}
	return nil
}

// isStructContainer reports whether t binds as a kwargs container. The
// package payload types (and other well-known opaque structs) bind as single
// positional values instead.
func isStructContainer(t reflect.Type) bool {
	base := t
	if base.Kind() == reflect.Pointer {
		base = base.Elem()
	}
	if base.Kind() != reflect.Struct {
		return false
	}
	switch base {
	case reflect.TypeFor[Value](), reflect.TypeFor[Date](), reflect.TypeFor[DateTime](),
		reflect.TypeFor[TimeDelta](), reflect.TypeFor[TimeZone](), reflect.TypeFor[NamedTuple](),
		reflect.TypeFor[DataclassValue](), reflect.TypeFor[Exception](),
		reflect.TypeFor[time.Time](), reflect.TypeFor[big.Int]():
		return false
	}
	return true
}

func (f *Function) paramName(i int) string {
	if i < len(f.paramNames) {
		return f.paramNames[i]
	}
	return fmt.Sprintf("arg%d", i+1)
}

// call invokes the handler for one Python call.
func (f *Function) call(ctx context.Context, args []Value, kwargs map[string]Value) (Value, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if f.rawHandler != nil {
		return f.rawHandler(ctx, args, kwargs)
	}
	if !f.handler.IsValid() || f.handler.Kind() != reflect.Func {
		return Value{}, fmt.Errorf("monty: function %q is not callable", f.name)
	}
	callArgs := make([]reflect.Value, 0, f.handlerType.NumIn())
	if f.takesContext {
		callArgs = append(callArgs, reflect.ValueOf(ctx))
	}
	bound, err := f.bindArgs(args, kwargs)
	if err != nil {
		return Value{}, err
	}
	callArgs = append(callArgs, bound...)
	callResults := f.handler.Call(callArgs)
	if len(callResults) == 0 {
		return None(), nil
	}
	if f.returnsErrorOnly {
		if callResults[0].IsNil() {
			return None(), nil
		}
		return Value{}, callResults[0].Interface().(error) //nolint:errcheck // validateHandler guarantees Out(0) implements error
	}
	if len(callResults) > 1 && !callResults[1].IsNil() {
		err, ok := callResults[1].Interface().(error)
		if !ok {
			return Value{}, fmt.Errorf("monty: second return value from %q is not an error", f.name)
		}
		return Value{}, err
	}
	return From(callResults[0].Interface())
}

// bindArgs binds the Python call to the handler's Python-visible parameters.
func (f *Function) bindArgs(args []Value, kwargs map[string]Value) ([]reflect.Value, error) {
	switch {
	case f.structInput:
		input, err := bindStructInput(f.inputType, args, kwargs, f.inputFields, f.inputNameToField)
		if err != nil {
			return nil, err
		}
		return []reflect.Value{input}, nil
	case len(f.params) == 0:
		if len(args) != 0 || len(kwargs) != 0 {
			return nil, fmt.Errorf("monty: %s() takes no arguments", f.name)
		}
		return nil, nil
	default:
		return f.bindPositional(args, kwargs)
	}
}

func (f *Function) bindPositional(args []Value, kwargs map[string]Value) ([]reflect.Value, error) {
	if len(args) > len(f.params) {
		return nil, fmt.Errorf("monty: %s() takes %d arguments, got %d", f.name, len(f.params), len(args))
	}
	bound := make([]reflect.Value, len(f.params))
	for i := range args {
		converted, err := valueAsReflect(args[i], f.params[i])
		if err != nil {
			return nil, fmt.Errorf("monty: %s() argument %s: %w", f.name, f.paramName(i), err)
		}
		bound[i] = converted
	}
	for name, value := range kwargs {
		index := -1
		for i := range f.params {
			if f.paramName(i) == name {
				index = i
				break
			}
		}
		if index < 0 {
			return nil, fmt.Errorf("monty: %s() got an unexpected keyword argument %q", f.name, name)
		}
		if index < len(args) {
			return nil, fmt.Errorf("monty: %s() got multiple values for argument %q", f.name, name)
		}
		if bound[index].IsValid() {
			return nil, fmt.Errorf("monty: %s() got multiple values for argument %q", f.name, name)
		}
		converted, err := valueAsReflect(value, f.params[index])
		if err != nil {
			return nil, fmt.Errorf("monty: %s() argument %s: %w", f.name, name, err)
		}
		bound[index] = converted
	}
	for i := range bound {
		if !bound[i].IsValid() {
			return nil, fmt.Errorf("monty: %s() missing argument %s", f.name, f.paramName(i))
		}
	}
	return bound, nil
}

// bindStructInput populates a kwargs-container struct from positional and
// keyword arguments.
func bindStructInput(target reflect.Type, args []Value, kwargs map[string]Value, fields []taggedField, nameToField map[string]taggedField) (reflect.Value, error) {
	if target.Kind() == reflect.Pointer {
		elem, err := bindStructInput(target.Elem(), args, kwargs, fields, nameToField)
		if err != nil {
			return reflect.Value{}, err
		}
		ptr := reflect.New(target.Elem())
		ptr.Elem().Set(elem)
		return ptr, nil
	}
	result := reflect.New(target).Elem()
	for i := range args {
		if i >= len(fields) {
			return reflect.Value{}, fmt.Errorf("monty: too many positional args for %s", target)
		}
		value, err := valueAsReflect(args[i], fields[i].fieldType)
		if err != nil {
			return reflect.Value{}, err
		}
		result.FieldByIndex(fields[i].index).Set(value)
	}
	for name, kwarg := range kwargs {
		field, ok := nameToField[name]
		if !ok {
			return reflect.Value{}, fmt.Errorf("monty: unexpected keyword arg %q", name)
		}
		value, err := valueAsReflect(kwarg, field.fieldType)
		if err != nil {
			return reflect.Value{}, err
		}
		result.FieldByIndex(field.index).Set(value)
	}
	return result, nil
}

// --------------------------------------------------------------------------
// Stub generation
// --------------------------------------------------------------------------

type structDef struct {
	name   string
	fields []taggedField
}

// collectStructDefs returns a TypedDict definition for every named struct type
// referenced by the stub, in dependency order (a referenced type precedes the
// type that references it). The kwargs-container struct itself is not defined —
// its fields become function parameters — but named structs nested inside its
// fields are, and so is the output struct and anything it references.
// Anonymous structs have no name to reference and render inline as
// dict[str, Any] (see pythonType), so they get no definition.
func (f *Function) collectStructDefs() []structDef {
	seen := map[string]bool{}
	var defs []structDef
	var visit func(t reflect.Type)
	visit = func(t reflect.Type) {
		if t == nil {
			return
		}
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			visit(t.Elem())
		case reflect.Map:
			visit(t.Key())
			visit(t.Elem())
		case reflect.Struct:
			if !isStructContainer(t) {
				return
			}
			fields := taggedFieldsFor(t).fields
			if t.Name() == "" {
				// Anonymous structs render as dict[str, Any]; only recurse for
				// named structs nested in their fields.
				for _, field := range fields {
					visit(field.fieldType)
				}
				return
			}
			if seen[t.Name()] {
				return
			}
			seen[t.Name()] = true
			for _, field := range fields {
				visit(field.fieldType)
			}
			defs = append(defs, structDef{name: t.Name(), fields: fields})
		default:
			// Scalars and other kinds reference no named struct types.
		}
	}
	// Input fields become parameters, so visit their types (not the container
	// struct itself); the output type does become a TypedDict.
	for _, field := range f.inputFields {
		visit(field.fieldType)
	}
	for _, param := range f.params {
		visit(param)
	}
	visit(f.outputType)
	return defs
}

// PythonStub returns the generated Python type stub for this host function
// (or the WithStub override).
func (f *Function) PythonStub() string {
	if f.stub != "" {
		return f.stub
	}
	if f.rawHandler != nil {
		var b strings.Builder
		b.WriteString("from typing import Any\n\n")
		if f.doc != "" {
			fmt.Fprintf(&b, "# %s\n", f.doc)
		}
		fmt.Fprintf(&b, "%s %s(*args: Any, **kwargs: Any) -> Any: ...", f.defKeyword(), f.name)
		return b.String()
	}
	defs := f.collectStructDefs()

	var outputName string
	switch {
	case f.returnsErrorOnly:
		outputName = "None"
	case len(f.outputFields) > 0:
		outputType := f.outputType
		if outputType != nil && outputType.Kind() == reflect.Pointer {
			outputType = outputType.Elem()
		}
		if outputType != nil && outputType.Name() != "" {
			outputName = outputType.Name() // defined by collectStructDefs
		} else {
			// Anonymous output struct: synthesize a TypedDict named Return. Its
			// referenced types were already collected from the output fields.
			outputName = "Return"
			defs = append(defs, structDef{name: "Return", fields: f.outputFields})
		}
	default:
		outputName = pythonType(f.outputType)
	}

	var b strings.Builder
	if len(defs) > 0 {
		b.WriteString("from typing import Any, TypedDict\n\n")
	} else {
		b.WriteString("from typing import Any\n\n")
	}
	for _, def := range defs {
		fmt.Fprintf(&b, "class %s(TypedDict):\n", def.name)
		for _, field := range def.fields {
			fmt.Fprintf(&b, "    %s: %s\n", field.name, pythonType(field.fieldType))
		}
		b.WriteByte('\n')
	}
	if f.doc != "" {
		fmt.Fprintf(&b, "# %s\n", f.doc)
	}
	var params []string
	switch {
	case f.structInput:
		params = make([]string, 0, len(f.inputFields))
		for _, field := range f.inputFields {
			params = append(params, fmt.Sprintf("%s: %s", field.name, pythonType(field.fieldType)))
		}
	default:
		params = make([]string, 0, len(f.params))
		for i, param := range f.params {
			params = append(params, fmt.Sprintf("%s: %s", f.paramName(i), pythonType(param)))
		}
	}
	fmt.Fprintf(&b, "%s %s(%s) -> %s: ...", f.defKeyword(), f.name, strings.Join(params, ", "), outputName)
	return b.String()
}

func (f *Function) defKeyword() string {
	if f.async {
		return "async def"
	}
	return "def"
}

func pythonType(t reflect.Type) string {
	if t == nil {
		return "Any"
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t {
	case reflect.TypeFor[Value]():
		return "Any"
	case reflect.TypeFor[Date]():
		return "date"
	case reflect.TypeFor[DateTime](), reflect.TypeFor[time.Time]():
		return "datetime"
	case reflect.TypeFor[TimeDelta](), reflect.TypeFor[time.Duration]():
		return "timedelta"
	case reflect.TypeFor[Path]():
		return "Path"
	case reflect.TypeFor[big.Int]():
		return "int"
	}
	switch t.Kind() {
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return "int"
	case reflect.Float32, reflect.Float64:
		return "float"
	case reflect.String:
		return "str"
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return "bytes"
		}
		return "list[" + pythonType(t.Elem()) + "]"
	case reflect.Map:
		return "dict[" + pythonType(t.Key()) + ", " + pythonType(t.Elem()) + "]"
	case reflect.Struct:
		if t.Name() != "" {
			return t.Name()
		}
		return "dict[str, Any]"
	default:
		return "Any"
	}
}

// --------------------------------------------------------------------------
// Host-callback fast path
// --------------------------------------------------------------------------

func isSignedIntType(typ reflect.Type) bool {
	if typ == nil {
		return false
	}
	switch typ.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return true
	default:
		return false
	}
}

// directRawCall type-switches the handler's concrete signature for the most
// common scalar shapes, eliminating reflect.Call (and its argument boxing)
// from the hot host-callback path entirely.
func (f *Function) directRawCall() func(context.Context, ffi.RawValue, ffi.RawValue) (ffi.RawValue, error) {
	if !f.handler.IsValid() || f.handler.Kind() != reflect.Func {
		return nil
	}
	bindInt := func(args, kwargs ffi.RawValue) (int64, error) {
		if Kind(args.Kind) != ListKind || Kind(kwargs.Kind) != DictKind {
			return 0, fmt.Errorf("monty: malformed raw call payload")
		}
		if kwargs.Len != 0 || args.Len != 1 {
			return 0, fmt.Errorf("monty: %s() takes a single positional argument", f.name)
		}
		if args.Ptr == nil {
			return 0, fmt.Errorf("monty: raw args pointer is null")
		}
		item := (*ffi.RawValue)(args.Ptr)
		if Kind(item.Kind) != IntKind {
			return 0, fmt.Errorf("monty: cannot assign %s to int", Kind(item.Kind))
		}
		return item.Int, nil
	}
	switch fn := f.handler.Interface().(type) {
	case func(int) int:
		return func(_ context.Context, args, kwargs ffi.RawValue) (ffi.RawValue, error) {
			n, err := bindInt(args, kwargs)
			if err != nil {
				return ffi.RawValue{}, err
			}
			return ffi.RawValue{Kind: ffi.KindInt, Int: int64(fn(int(n)))}, nil
		}
	case func(int) (int, error):
		return func(_ context.Context, args, kwargs ffi.RawValue) (ffi.RawValue, error) {
			n, err := bindInt(args, kwargs)
			if err != nil {
				return ffi.RawValue{}, err
			}
			result, err := fn(int(n))
			if err != nil {
				return ffi.RawValue{}, err
			}
			return ffi.RawValue{Kind: ffi.KindInt, Int: int64(result)}, nil
		}
	case func(context.Context, int) int:
		return func(ctx context.Context, args, kwargs ffi.RawValue) (ffi.RawValue, error) {
			n, err := bindInt(args, kwargs)
			if err != nil {
				return ffi.RawValue{}, err
			}
			return ffi.RawValue{Kind: ffi.KindInt, Int: int64(fn(ctx, int(n)))}, nil
		}
	case func(context.Context, int) (int, error):
		return func(ctx context.Context, args, kwargs ffi.RawValue) (ffi.RawValue, error) {
			n, err := bindInt(args, kwargs)
			if err != nil {
				return ffi.RawValue{}, err
			}
			result, err := fn(ctx, int(n))
			if err != nil {
				return ffi.RawValue{}, err
			}
			return ffi.RawValue{Kind: ffi.KindInt, Int: int64(result)}, nil
		}
	case func(int64) int64:
		return func(_ context.Context, args, kwargs ffi.RawValue) (ffi.RawValue, error) {
			n, err := bindInt(args, kwargs)
			if err != nil {
				return ffi.RawValue{}, err
			}
			return ffi.RawValue{Kind: ffi.KindInt, Int: fn(n)}, nil
		}
	case func(int64) (int64, error):
		return func(_ context.Context, args, kwargs ffi.RawValue) (ffi.RawValue, error) {
			n, err := bindInt(args, kwargs)
			if err != nil {
				return ffi.RawValue{}, err
			}
			result, err := fn(n)
			if err != nil {
				return ffi.RawValue{}, err
			}
			return ffi.RawValue{Kind: ffi.KindInt, Int: result}, nil
		}
	default:
		return nil
	}
}

// fastReflectRawCall returns a direct raw-FFI handler when the signature
// qualifies (signed-int scalar or struct of signed ints, returning a signed
// int); nil otherwise. The fast path skips Value materialization entirely.
func (f *Function) fastReflectRawCall() func(context.Context, ffi.RawValue, ffi.RawValue) (ffi.RawValue, error) {
	if direct := f.directRawCall(); direct != nil {
		return direct
	}
	if !f.handler.IsValid() || f.handler.Kind() != reflect.Func {
		return nil
	}
	if !isSignedIntType(f.outputType) {
		return nil
	}
	handlerType := f.handlerType
	if handlerType.NumOut() == 0 || handlerType.NumOut() > 2 {
		return nil
	}
	if handlerType.NumOut() == 2 && !handlerType.Out(1).Implements(reflect.TypeFor[error]()) {
		return nil
	}

	var scalarInput bool
	var inputType reflect.Type
	switch {
	case f.structInput:
		for _, field := range f.inputFields {
			if !isSignedIntType(field.fieldType) {
				return nil
			}
		}
		inputType = f.inputType
	case len(f.params) == 1 && isSignedIntType(f.params[0]):
		scalarInput = true
		inputType = f.params[0]
	default:
		return nil
	}

	takesContext := f.takesContext
	returnsError := handlerType.NumOut() == 2
	handler := f.handler
	fields := f.inputFields
	nameToField := f.inputNameToField
	name := f.name
	return func(ctx context.Context, args ffi.RawValue, kwargs ffi.RawValue) (ffi.RawValue, error) {
		input := reflect.New(inputType).Elem()
		if scalarInput {
			if err := bindRawScalarIntInput(input, args, kwargs); err != nil {
				return ffi.RawValue{}, err
			}
		} else if err := bindRawStructInput(input, args, kwargs, fields, nameToField); err != nil {
			return ffi.RawValue{}, err
		}
		var results []reflect.Value
		if takesContext {
			if ctx == nil {
				ctx = context.Background()
			}
			callArgs := [2]reflect.Value{reflect.ValueOf(ctx), input}
			results = handler.Call(callArgs[:])
		} else {
			callArgs := [1]reflect.Value{input}
			results = handler.Call(callArgs[:])
		}
		if returnsError && !results[1].IsNil() {
			err, ok := results[1].Interface().(error)
			if !ok {
				return ffi.RawValue{}, fmt.Errorf("monty: second return value from %q is not an error", name)
			}
			return ffi.RawValue{}, err
		}
		return ffi.RawValue{Kind: ffi.KindInt, Int: results[0].Int()}, nil
	}
}

// bindRawScalarIntInput binds a non-struct integer input: exactly one
// positional argument and no keywords (zero arguments leave the zero value).
func bindRawScalarIntInput(target reflect.Value, args ffi.RawValue, kwargs ffi.RawValue) error {
	if Kind(args.Kind) != ListKind {
		return fmt.Errorf("monty: expected positional args list, got %s", Kind(args.Kind))
	}
	if Kind(kwargs.Kind) != DictKind {
		return fmt.Errorf("monty: expected keyword args dict, got %s", Kind(kwargs.Kind))
	}
	if kwargs.Len != 0 || args.Len > 1 {
		return fmt.Errorf("monty: %s takes a single positional argument", target.Type())
	}
	if args.Len == 0 {
		return nil
	}
	if args.Ptr == nil {
		return fmt.Errorf("monty: raw args pointer is null")
	}
	return setRawIntField(target, *(*ffi.RawValue)(args.Ptr))
}

func bindRawStructInput(target reflect.Value, args ffi.RawValue, kwargs ffi.RawValue, fields []taggedField, nameToField map[string]taggedField) error {
	if Kind(args.Kind) != ListKind {
		return fmt.Errorf("monty: expected positional args list, got %s", Kind(args.Kind))
	}
	if args.Len != 0 {
		if args.Ptr == nil {
			return fmt.Errorf("monty: raw args pointer is null")
		}
		values := unsafe.Slice((*ffi.RawValue)(args.Ptr), args.Len)
		for i := range values {
			if i >= len(fields) {
				return fmt.Errorf("monty: too many positional args for %s", target.Type())
			}
			if err := setRawIntField(target.FieldByIndex(fields[i].index), values[i]); err != nil {
				return err
			}
		}
	}
	if Kind(kwargs.Kind) != DictKind {
		return fmt.Errorf("monty: expected keyword args dict, got %s", Kind(kwargs.Kind))
	}
	if kwargs.Len == 0 {
		return nil
	}
	if kwargs.Ptr == nil {
		return fmt.Errorf("monty: raw kwargs pointer is null")
	}
	pairs := unsafe.Slice((*ffi.RawPair)(kwargs.Ptr), kwargs.Len)
	for i := range pairs {
		key := pairs[i].Key
		if Kind(key.Kind) != StringKind {
			return fmt.Errorf("monty: keyword arg key must be string")
		}
		name := ""
		if key.Ptr != nil && key.Len != 0 {
			name = unsafe.String((*byte)(key.Ptr), int(key.Len))
		}
		field, ok := nameToField[name]
		if !ok {
			return fmt.Errorf("monty: unexpected keyword arg %q", name)
		}
		if err := setRawIntField(target.FieldByIndex(field.index), pairs[i].Value); err != nil {
			return err
		}
	}
	return nil
}

func setRawIntField(field reflect.Value, raw ffi.RawValue) error {
	if Kind(raw.Kind) != IntKind {
		return fmt.Errorf("monty: cannot assign %s to %s", Kind(raw.Kind), field.Type())
	}
	if field.OverflowInt(raw.Int) {
		return fmt.Errorf("monty: cannot assign %d to %s", raw.Int, field.Type())
	}
	field.SetInt(raw.Int)
	return nil
}
