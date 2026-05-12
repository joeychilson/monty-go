package monty

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/joeychilson/monty/internal/ffi"
)

// Function exposes a Go function to Monty Python code.
type Function struct {
	name             string
	doc              string
	handler          reflect.Value
	handlerType      reflect.Type
	fastCall         func(context.Context, []Value, []Pair) (Value, error)
	fastRawCall      func(context.Context, ffi.RawValue, ffi.RawValue) (ffi.RawValue, error)
	inputType        reflect.Type
	outputType       reflect.Type
	takesContext     bool
	inputFields      []montyField
	inputNameToField map[string]montyField
	outputFields     []montyField
}

// FunctionOption configures a Function.
type FunctionOption func(*Function)

// WithDoc sets the Python doc comment used in the generated stub for a Function.
func WithDoc(text string) FunctionOption {
	return func(f *Function) { f.doc = text }
}

// NewFunction creates a named Go host function callable from Monty Python code.
//
// handler may optionally accept context.Context as its first argument. The
// next argument, when present, is populated from positional and keyword Python
// arguments. If handler returns two values, the second must implement error.
func NewFunction(name string, handler any, opts ...FunctionOption) *Function {
	f := &Function{name: name, handler: reflect.ValueOf(handler)}
	for _, opt := range opts {
		opt(f)
	}
	f.fastCall, f.fastRawCall = fastFunctionCall(handler)
	f.inspect()
	if f.fastRawCall == nil {
		f.fastRawCall = f.fastReflectRawCall()
	}
	return f
}

// Name returns the Python-visible function name.
func (f *Function) Name() string { return f.name }

// PythonStub returns the generated Python type stub for this host function.
func (f *Function) PythonStub() string {
	var b strings.Builder
	var outputName string
	if len(f.outputFields) > 0 {
		outputType := f.outputType
		if outputType != nil && outputType.Kind() == reflect.Pointer {
			outputType = outputType.Elem()
		}
		outputName = "Return"
		if outputType != nil && outputType.Name() != "" {
			outputName = outputType.Name()
		}
		b.WriteString("from typing import Any, TypedDict\n\n")
		fmt.Fprintf(&b, "class %s(TypedDict):\n", outputName)
		for _, field := range f.outputFields {
			fmt.Fprintf(&b, "    %s: %s\n", field.name, pythonType(field.fieldType))
		}
		b.WriteByte('\n')
	} else {
		b.WriteString("from typing import Any\n\n")
		outputName = pythonType(f.outputType)
	}
	if f.doc != "" {
		fmt.Fprintf(&b, "# %s\n", f.doc)
	}
	params := make([]string, 0, len(f.inputFields))
	for _, field := range f.inputFields {
		params = append(params, fmt.Sprintf("%s: %s", field.name, pythonType(field.fieldType)))
	}
	fmt.Fprintf(&b, "def %s(%s) -> %s: ...", f.name, strings.Join(params, ", "), outputName)
	return b.String()
}

func (f *Function) inspect() {
	if !f.handler.IsValid() || f.handler.Kind() != reflect.Func {
		return
	}
	functionType := f.handler.Type()
	f.handlerType = functionType
	inputIndex := 0
	if functionType.NumIn() > 0 && functionType.In(0) == reflect.TypeFor[context.Context]() {
		f.takesContext = true
		inputIndex = 1
	}
	if inputIndex < functionType.NumIn() {
		f.inputType = functionType.In(inputIndex)
	}
	if functionType.NumOut() > 0 {
		f.outputType = functionType.Out(0)
	}
	if f.inputType != nil {
		inputInfo := montyFieldInfoFor(f.inputType)
		f.inputFields = inputInfo.fields
		f.inputNameToField = inputInfo.nameToField
	}
	if f.outputType != nil {
		f.outputFields = montyFieldInfoFor(f.outputType).fields
	}
}

func (f *Function) call(ctx context.Context, args []Value, kwargs []Pair) (Value, error) {
	if f.fastCall != nil {
		return f.fastCall(ctx, args, kwargs)
	}
	if !f.handler.IsValid() || f.handler.Kind() != reflect.Func {
		return Value{}, fmt.Errorf("monty: function %q is not callable", f.name)
	}
	callArgs := make([]reflect.Value, 0, f.handlerType.NumIn())
	if f.takesContext {
		if ctx == nil {
			ctx = context.Background()
		}
		callArgs = append(callArgs, reflect.ValueOf(ctx))
	}
	if f.inputType != nil {
		inputValue, err := bindInput(f.inputType, args, kwargs, f.inputFields, f.inputNameToField)
		if err != nil {
			return Value{}, err
		}
		callArgs = append(callArgs, inputValue)
	}
	callResults := f.handler.Call(callArgs)
	if len(callResults) == 0 {
		return None(), nil
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

func fastFunctionCall(handler any) (
	func(context.Context, []Value, []Pair) (Value, error),
	func(context.Context, ffi.RawValue, ffi.RawValue) (ffi.RawValue, error),
) {
	switch fn := handler.(type) {
	case func(int) (int, error):
		fastCall := func(_ context.Context, args []Value, kwargs []Pair) (Value, error) {
			value, err := bindFastIntArg(args, kwargs)
			if err != nil {
				return Value{}, err
			}
			result, err := fn(value)
			if err != nil {
				return Value{}, err
			}
			return Int(result), nil
		}
		fastRawCall := func(_ context.Context, args ffi.RawValue, kwargs ffi.RawValue) (ffi.RawValue, error) {
			value, err := bindFastRawIntArg(args, kwargs)
			if err != nil {
				return ffi.RawValue{}, err
			}
			result, err := fn(value)
			if err != nil {
				return ffi.RawValue{}, err
			}
			return ffi.RawValue{Kind: ffi.KindInt, Int: int64(result)}, nil
		}
		return fastCall, fastRawCall
	case func(context.Context, int) (int, error):
		fastCall := func(ctx context.Context, args []Value, kwargs []Pair) (Value, error) {
			value, err := bindFastIntArg(args, kwargs)
			if err != nil {
				return Value{}, err
			}
			if ctx == nil {
				ctx = context.Background()
			}
			result, err := fn(ctx, value)
			if err != nil {
				return Value{}, err
			}
			return Int(result), nil
		}
		fastRawCall := func(ctx context.Context, args ffi.RawValue, kwargs ffi.RawValue) (ffi.RawValue, error) {
			value, err := bindFastRawIntArg(args, kwargs)
			if err != nil {
				return ffi.RawValue{}, err
			}
			if ctx == nil {
				ctx = context.Background()
			}
			result, err := fn(ctx, value)
			if err != nil {
				return ffi.RawValue{}, err
			}
			return ffi.RawValue{Kind: ffi.KindInt, Int: int64(result)}, nil
		}
		return fastCall, fastRawCall
	case func(int) int:
		fastCall := func(_ context.Context, args []Value, kwargs []Pair) (Value, error) {
			value, err := bindFastIntArg(args, kwargs)
			if err != nil {
				return Value{}, err
			}
			return Int(fn(value)), nil
		}
		fastRawCall := func(_ context.Context, args ffi.RawValue, kwargs ffi.RawValue) (ffi.RawValue, error) {
			value, err := bindFastRawIntArg(args, kwargs)
			if err != nil {
				return ffi.RawValue{}, err
			}
			return ffi.RawValue{Kind: ffi.KindInt, Int: int64(fn(value))}, nil
		}
		return fastCall, fastRawCall
	case func(context.Context, int) int:
		fastCall := func(ctx context.Context, args []Value, kwargs []Pair) (Value, error) {
			value, err := bindFastIntArg(args, kwargs)
			if err != nil {
				return Value{}, err
			}
			if ctx == nil {
				ctx = context.Background()
			}
			return Int(fn(ctx, value)), nil
		}
		fastRawCall := func(ctx context.Context, args ffi.RawValue, kwargs ffi.RawValue) (ffi.RawValue, error) {
			value, err := bindFastRawIntArg(args, kwargs)
			if err != nil {
				return ffi.RawValue{}, err
			}
			if ctx == nil {
				ctx = context.Background()
			}
			return ffi.RawValue{Kind: ffi.KindInt, Int: int64(fn(ctx, value))}, nil
		}
		return fastCall, fastRawCall
	default:
		return nil, nil
	}
}

func bindFastIntArg(args []Value, kwargs []Pair) (int, error) {
	if len(kwargs) != 0 {
		return 0, fmt.Errorf("monty: int fast path does not accept keyword args")
	}
	if len(args) != 1 {
		return 0, fmt.Errorf("monty: expected 1 positional arg, got %d", len(args))
	}
	if args[0].kind != IntKind {
		return 0, fmt.Errorf("monty: cannot assign %s to int", args[0].kind)
	}
	return args[0].Int(), nil
}

func bindFastRawIntArg(args ffi.RawValue, kwargs ffi.RawValue) (int, error) {
	if Kind(kwargs.Kind) == DictKind && kwargs.Len != 0 {
		return 0, fmt.Errorf("monty: int fast path does not accept keyword args")
	}
	if Kind(args.Kind) != ListKind {
		return 0, fmt.Errorf("monty: expected positional args list, got %s", Kind(args.Kind))
	}
	if args.Len != 1 {
		return 0, fmt.Errorf("monty: expected 1 positional arg, got %d", args.Len)
	}
	if args.Ptr == nil {
		return 0, fmt.Errorf("monty: raw args pointer is null")
	}
	values := unsafe.Slice((*ffi.RawValue)(args.Ptr), args.Len)
	if Kind(values[0].Kind) != IntKind {
		return 0, fmt.Errorf("monty: cannot assign %s to int", Kind(values[0].Kind))
	}
	return int(values[0].Int), nil
}

func (f *Function) fastReflectRawCall() func(context.Context, ffi.RawValue, ffi.RawValue) (ffi.RawValue, error) {
	if !f.handler.IsValid() || f.handler.Kind() != reflect.Func || f.inputType == nil {
		return nil
	}
	if f.inputType.Kind() != reflect.Struct || !isSignedIntType(f.outputType) {
		return nil
	}
	handlerType := f.handlerType
	if handlerType.NumOut() == 0 || handlerType.NumOut() > 2 {
		return nil
	}
	if handlerType.NumOut() == 2 && !handlerType.Out(1).Implements(reflect.TypeFor[error]()) {
		return nil
	}
	for _, field := range f.inputFields {
		if !isSignedIntType(field.fieldType) {
			return nil
		}
	}
	takesContext := f.takesContext
	returnsError := handlerType.NumOut() == 2
	inputType := f.inputType
	handler := f.handler
	fields := f.inputFields
	nameToField := f.inputNameToField
	return func(ctx context.Context, args ffi.RawValue, kwargs ffi.RawValue) (ffi.RawValue, error) {
		input := reflect.New(inputType).Elem()
		if err := bindRawStructInput(input, args, kwargs, fields, nameToField); err != nil {
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
				return ffi.RawValue{}, fmt.Errorf("monty: second return value from %q is not an error", f.name)
			}
			return ffi.RawValue{}, err
		}
		return ffi.RawValue{Kind: ffi.KindInt, Int: results[0].Int()}, nil
	}
}

func bindRawStructInput(target reflect.Value, args ffi.RawValue, kwargs ffi.RawValue, fields []montyField, nameToField map[string]montyField) error {
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

func bindInput(target reflect.Type, args []Value, kwargs []Pair, fields []montyField, nameToField map[string]montyField) (reflect.Value, error) {
	if target.Kind() == reflect.Pointer {
		value, err := bindInput(target.Elem(), args, kwargs, fields, nameToField)
		if err != nil {
			return reflect.Value{}, err
		}
		ptr := reflect.New(target.Elem())
		ptr.Elem().Set(value)
		return ptr, nil
	}
	if target.Kind() != reflect.Struct {
		if len(args) == 0 {
			return reflect.Zero(target), nil
		}
		return valueAsReflect(args[0], target)
	}
	result := reflect.New(target).Elem()
	if fields == nil && nameToField == nil {
		info := montyFieldInfoFor(target)
		fields = info.fields
		nameToField = info.nameToField
	}
	for i, arg := range args {
		if i >= len(fields) {
			return reflect.Value{}, fmt.Errorf("monty: too many positional args for %s", target)
		}
		field := result.FieldByIndex(fields[i].index)
		value, err := valueAsReflect(arg, fields[i].fieldType)
		if err != nil {
			return reflect.Value{}, err
		}
		field.Set(value)
	}
	for pairIndex := range kwargs {
		pair := &kwargs[pairIndex]
		if pair.Key.kind != StringKind {
			return reflect.Value{}, fmt.Errorf("monty: keyword arg key must be string")
		}
		fieldInfo, ok := nameToField[pair.Key.text]
		if !ok {
			return reflect.Value{}, fmt.Errorf("monty: unexpected keyword arg %q", pair.Key.text)
		}
		field := result.FieldByIndex(fieldInfo.index)
		value, err := valueAsReflect(pair.Value, fieldInfo.fieldType)
		if err != nil {
			return reflect.Value{}, err
		}
		field.Set(value)
	}
	return result, nil
}

func valueAsReflect(value Value, target reflect.Type) (reflect.Value, error) {
	if target == reflect.TypeFor[Value]() {
		return reflect.ValueOf(value), nil
	}
	switch target {
	case reflect.TypeFor[MontyDate]():
		if value.kind != DateKind {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to MontyDate", value.kind)
		}
		return reflect.ValueOf(value.Date()), nil
	case reflect.TypeFor[MontyDateTime]():
		if value.kind != DateTimeKind {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to MontyDateTime", value.kind)
		}
		return reflect.ValueOf(value.DateTime()), nil
	case reflect.TypeFor[MontyTimeDelta]():
		if value.kind != TimeDeltaKind {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to MontyTimeDelta", value.kind)
		}
		return reflect.ValueOf(value.TimeDelta()), nil
	case reflect.TypeFor[MontyTimeZone]():
		if value.kind != TimeZoneKind {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to MontyTimeZone", value.kind)
		}
		return reflect.ValueOf(value.TimeZone()), nil
	case reflect.TypeFor[MontyNamedTuple]():
		if value.kind != NamedTupleKind {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to MontyNamedTuple", value.kind)
		}
		return reflect.ValueOf(value.NamedTuple()), nil
	case reflect.TypeFor[MontyDataclass]():
		if value.kind != DataclassKind {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to MontyDataclass", value.kind)
		}
		return reflect.ValueOf(value.Dataclass()), nil
	case reflect.TypeFor[time.Time]():
		switch value.kind {
		case DateTimeKind:
			return reflect.ValueOf(value.DateTime().Time()), nil
		case DateKind:
			return reflect.ValueOf(value.Date().Time()), nil
		default:
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to time.Time", value.kind)
		}
	case reflect.TypeFor[time.Duration]():
		if value.kind != TimeDeltaKind {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to time.Duration", value.kind)
		}
		duration, ok := value.TimeDelta().Duration()
		if !ok {
			return reflect.Value{}, fmt.Errorf("monty: timedelta overflows time.Duration")
		}
		return reflect.ValueOf(duration), nil
	}
	if target.Kind() == reflect.Pointer {
		if value.kind == NoneKind {
			return reflect.Zero(target), nil
		}
		converted, err := valueAsReflect(value, target.Elem())
		if err != nil {
			return reflect.Value{}, err
		}
		result := reflect.New(target.Elem())
		result.Elem().Set(converted)
		return result, nil
	}
	if target.Kind() == reflect.Interface {
		native := reflect.ValueOf(value.Interface())
		if !native.IsValid() {
			return reflect.Zero(target), nil
		}
		if native.Type().AssignableTo(target) {
			return native, nil
		}
		if native.Type().ConvertibleTo(target) {
			return native.Convert(target), nil
		}
		return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to %s", native.Type(), target)
	}
	result := reflect.New(target).Elem()
	switch target.Kind() {
	case reflect.String:
		if !isStringLikeKind(value.kind) {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to string", value.kind)
		}
		result.SetString(value.Str())
	case reflect.Bool:
		if value.kind != BoolKind {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to bool", value.kind)
		}
		result.SetBool(value.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if value.kind != IntKind {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to %s", value.kind, target)
		}
		signed := value.Int64()
		if target.Bits() < 64 {
			limit := int64(1) << (target.Bits() - 1)
			if signed < -limit || signed >= limit {
				return reflect.Value{}, fmt.Errorf("monty: cannot assign %d to %s", signed, target)
			}
		}
		result.SetInt(signed)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		if value.kind != IntKind {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to %s", value.kind, target)
		}
		signed := value.Int64()
		if signed < 0 {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign negative int to %s", target)
		}
		if target.Bits() < 64 {
			limit := int64(1) << target.Bits()
			if signed >= limit {
				return reflect.Value{}, fmt.Errorf("monty: cannot assign %d to %s", signed, target)
			}
		}
		unsigned := uint64(signed)
		result.SetUint(unsigned)
	case reflect.Float32, reflect.Float64:
		switch value.kind {
		case IntKind:
			result.SetFloat(float64(value.Int64()))
		case FloatKind:
			result.SetFloat(value.Float64())
		default:
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to %s", value.kind, target)
		}
	case reflect.Slice:
		if target.Elem().Kind() == reflect.Uint8 && value.kind == BytesKind {
			result.SetBytes(value.Bytes())
			return result, nil
		}
		items, ok := sequenceItems(value)
		if !ok {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to slice", value.kind)
		}
		result = reflect.MakeSlice(target, len(items), len(items))
		for i, item := range items {
			converted, err := valueAsReflect(item, target.Elem())
			if err != nil {
				return reflect.Value{}, err
			}
			result.Index(i).Set(converted)
		}
	case reflect.Array:
		items, ok := sequenceItems(value)
		if !ok {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to array", value.kind)
		}
		if len(items) != target.Len() {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %d items to %s", len(items), target)
		}
		for i, item := range items {
			converted, err := valueAsReflect(item, target.Elem())
			if err != nil {
				return reflect.Value{}, err
			}
			result.Index(i).Set(converted)
		}
	case reflect.Map:
		if value.kind != DictKind && value.kind != DataclassKind {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to map", value.kind)
		}
		result = reflect.MakeMapWithSize(target, len(value.pairs))
		for i := range value.pairs {
			key, err := valueAsReflect(value.pairs[i].Key, target.Key())
			if err != nil {
				return reflect.Value{}, err
			}
			mapValue, err := valueAsReflect(value.pairs[i].Value, target.Elem())
			if err != nil {
				return reflect.Value{}, err
			}
			result.SetMapIndex(key, mapValue)
		}
	case reflect.Struct:
		if value.kind != DictKind && value.kind != DataclassKind {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to struct", value.kind)
		}
		info := montyFieldInfoFor(target)
		for pairIndex := range value.pairs {
			pair := &value.pairs[pairIndex]
			if pair.Key.kind != StringKind {
				continue
			}
			fieldInfo, ok := info.nameToField[pair.Key.text]
			if !ok {
				continue
			}
			field, err := valueAsReflect(pair.Value, fieldInfo.fieldType)
			if err != nil {
				return reflect.Value{}, err
			}
			result.FieldByIndex(fieldInfo.index).Set(field)
		}
	default:
		return reflect.Value{}, fmt.Errorf("monty: cannot assign Value to %s", target)
	}
	return result, nil
}

func sequenceItems(value Value) ([]Value, bool) {
	switch value.kind {
	case ListKind, TupleKind, NamedTupleKind, SetKind, FrozenSetKind:
		return value.items, true
	default:
		return nil, false
	}
}

type montyField struct {
	name      string
	index     []int
	fieldType reflect.Type
}

type montyFieldInfo struct {
	fields      []montyField
	nameToField map[string]montyField
}

var montyFieldCache sync.Map

func montyFieldInfoFor(t reflect.Type) montyFieldInfo {
	if t == nil {
		return montyFieldInfo{}
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return montyFieldInfo{}
	}
	if cached, ok := montyFieldCache.Load(t); ok {
		return cached.(montyFieldInfo) //nolint:errcheck // montyFieldCache only stores montyFieldInfo
	}
	fields := buildMontyFields(t)
	nameToField := make(map[string]montyField, len(fields))
	for _, field := range fields {
		nameToField[field.name] = field
	}
	info := montyFieldInfo{fields: fields, nameToField: nameToField}
	actual, _ := montyFieldCache.LoadOrStore(t, info)
	return actual.(montyFieldInfo) //nolint:errcheck // montyFieldCache only stores montyFieldInfo
}

func buildMontyFields(t reflect.Type) []montyField {
	if t == nil {
		return nil
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	fields := make([]montyField, 0, t.NumField())
	for field := range t.Fields() {
		if field.PkgPath != "" {
			continue
		}
		name, ok := fieldName(field)
		if ok {
			fields = append(fields, montyField{
				name:      name,
				index:     append([]int(nil), field.Index...),
				fieldType: field.Type,
			})
		}
	}
	return fields
}

func pythonType(t reflect.Type) string {
	if t == nil {
		return "Any"
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
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
