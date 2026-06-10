package monty

import (
	"fmt"
	"math"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// From converts a Go value into a Monty Value.
//
// Valuer implementations are honored first; otherwise scalars, strings,
// []byte, slices, arrays, maps, structs (using `monty` field tags with
// snake_case defaults), time.Time, time.Duration, and *big.Int convert by
// reflection. nil converts to None.
func From(value any) (Value, error) {
	return valueFromReflect(reflect.ValueOf(value), 0)
}

// MustFrom is From for literals and tests: it panics on conversion failure.
func MustFrom(value any) Value {
	converted, err := From(value)
	if err != nil {
		panic(err)
	}
	return converted
}

func valueFromReflect(reflectValue reflect.Value, depth int) (Value, error) {
	if depth > 100 {
		return Value{}, fmt.Errorf("monty: max conversion depth exceeded")
	}
	if !reflectValue.IsValid() {
		return None(), nil
	}
	if reflectValue.Type() == reflect.TypeFor[Value]() {
		value, ok := reflectValue.Interface().(Value)
		if !ok {
			return Value{}, fmt.Errorf("monty: expected Value, got %T", reflectValue.Interface())
		}
		return value, nil
	}
	if reflectValue.CanInterface() {
		switch value := reflectValue.Interface().(type) {
		case Valuer:
			return value.MontyValue(), nil
		case time.Time:
			return DateTimeOf(value).MontyValue(), nil
		case time.Duration:
			return TimeDeltaOf(value).MontyValue(), nil
		case big.Int:
			return bigIntValue(&value), nil
		case *big.Int:
			if value == nil {
				return None(), nil
			}
			return bigIntValue(value), nil
		}
	}
	if reflectValue.Kind() == reflect.Pointer || reflectValue.Kind() == reflect.Interface {
		if reflectValue.IsNil() {
			return None(), nil
		}
		return valueFromReflect(reflectValue.Elem(), depth+1)
	}
	switch reflectValue.Kind() {
	case reflect.Bool:
		return Bool(reflectValue.Bool()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return Int64(reflectValue.Int()), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		unsigned := reflectValue.Uint()
		if unsigned > math.MaxInt64 {
			return Value{kind: BigIntKind, text: strconv.FormatUint(unsigned, 10)}, nil
		}
		return Int64(int64(unsigned)), nil
	case reflect.Float32, reflect.Float64:
		return Float(reflectValue.Float()), nil
	case reflect.String:
		return Str(reflectValue.String()), nil
	case reflect.Slice:
		if reflectValue.Type().Elem().Kind() == reflect.Uint8 {
			return Bytes(reflectValue.Bytes()), nil
		}
		fallthrough
	case reflect.Array:
		items := make([]Value, reflectValue.Len())
		for i := range reflectValue.Len() {
			item, err := valueFromReflect(reflectValue.Index(i), depth+1)
			if err != nil {
				return Value{}, err
			}
			items[i] = item
		}
		return Value{kind: ListKind, items: items}, nil
	case reflect.Map:
		pairs := make([]Pair, 0, reflectValue.Len())
		for k, v := range reflectValue.Seq2() {
			key, err := valueFromReflect(k, depth+1)
			if err != nil {
				return Value{}, err
			}
			value, err := valueFromReflect(v, depth+1)
			if err != nil {
				return Value{}, err
			}
			pairs = append(pairs, Pair{Key: key, Value: value})
		}
		return Value{kind: DictKind, pairs: pairs}, nil
	case reflect.Struct:
		return structToValue(reflectValue, depth+1)
	default:
		return Value{}, fmt.Errorf("monty: cannot convert %s to Value", reflectValue.Type())
	}
}

func bigIntValue(v *big.Int) Value {
	if v.IsInt64() {
		return Int64(v.Int64())
	}
	return Value{kind: BigIntKind, text: v.String()}
}

func structToValue(structValue reflect.Value, depth int) (Value, error) {
	pairs := make([]Pair, 0, structValue.NumField())
	for field, fieldValue := range structValue.Fields() {
		if field.PkgPath != "" {
			continue
		}
		name, ok := fieldName(field)
		if !ok {
			continue
		}
		value, err := valueFromReflect(fieldValue, depth+1)
		if err != nil {
			return Value{}, err
		}
		pairs = append(pairs, Pair{Key: Str(name), Value: value})
	}
	return Value{kind: DictKind, pairs: pairs}, nil
}

func fieldName(field reflect.StructField) (string, bool) {
	tag := field.Tag.Get("monty")
	if tag == "-" {
		return "", false
	}
	if name, _, _ := strings.Cut(tag, ","); name != "" {
		return name, true
	}
	return snakeCase(field.Name), true
}

// snakeCase converts a Go field name to a Python-style snake_case input name.
// It is acronym-aware and Unicode-aware: an underscore is inserted before an
// upper-case rune only when it begins a new word — i.e. the previous rune is
// not upper-case, or it ends an acronym (previous rune upper-case, next rune
// lower-case). So HTTPTimeout -> http_timeout, UserID -> user_id,
// APIKey2 -> api_key2, and an already-snake name is left unchanged.
func snakeCase(name string) string {
	runes := []rune(name)
	var b strings.Builder
	b.Grow(len(name) + 4)
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) {
			prevIsUpper := unicode.IsUpper(runes[i-1])
			nextIsLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if !prevIsUpper || nextIsLower {
				b.WriteByte('_')
			}
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// As converts a Monty Value into a Go value of type T.
//
// The conversion accepts primitive Go types, structs with monty tags, maps,
// slices, the package payload types, time.Time, time.Duration, *big.Int,
// and Value itself.
func As[T any](value Value) (T, error) {
	var zero T
	// In each arm below, T was just narrowed by the outer switch, so the inner
	// type assertions to T are infallible by construction.
	switch any(zero).(type) {
	case Value:
		return any(value).(T), nil //nolint:errcheck // T is Value here
	case int:
		if value.kind != IntKind {
			return zero, fmt.Errorf("monty: cannot convert %s to int", value.kind)
		}
		return any(int(value.intValue)).(T), nil //nolint:errcheck // T is int here
	case int64:
		if value.kind != IntKind {
			return zero, fmt.Errorf("monty: cannot convert %s to int64", value.kind)
		}
		return any(value.intValue).(T), nil //nolint:errcheck // T is int64 here
	case float64:
		switch value.kind {
		case IntKind:
			return any(float64(value.intValue)).(T), nil //nolint:errcheck // T is float64 here
		case FloatKind:
			return any(value.floatValue).(T), nil //nolint:errcheck // T is float64 here
		default:
			return zero, fmt.Errorf("monty: cannot convert %s to float64", value.kind)
		}
	case string:
		if !isStringLikeKind(value.kind) {
			return zero, fmt.Errorf("monty: cannot convert %s to string", value.kind)
		}
		return any(value.text).(T), nil //nolint:errcheck // T is string here
	case bool:
		if value.kind != BoolKind {
			return zero, fmt.Errorf("monty: cannot convert %s to bool", value.kind)
		}
		return any(value.boolValue).(T), nil //nolint:errcheck // T is bool here
	}
	converted, err := valueAsReflect(value, reflect.TypeFor[T]())
	if err != nil {
		return zero, err
	}
	result, ok := converted.Interface().(T)
	if !ok {
		return zero, fmt.Errorf("monty: converted value is %s, not requested type", converted.Type())
	}
	return result, nil
}

// Decode converts the value into dst, which must be a non-nil pointer. It is
// the encoding/json-style counterpart of As.
func (v Value) Decode(dst any) error {
	pointer := reflect.ValueOf(dst)
	if pointer.Kind() != reflect.Pointer || pointer.IsNil() {
		return fmt.Errorf("monty: Decode requires a non-nil pointer, got %T", dst)
	}
	converted, err := valueAsReflect(v, pointer.Type().Elem())
	if err != nil {
		return err
	}
	pointer.Elem().Set(converted)
	return nil
}

func isStringLikeKind(kind Kind) bool {
	switch kind {
	case StringKind, BigIntKind, PathKind, ReprKind, CycleKind, FunctionKind, ExceptionKind, TypeKind, BuiltinFunctionKind:
		return true
	default:
		return false
	}
}

//nolint:gocyclo // a single exhaustive conversion switch is clearer split apart
func valueAsReflect(value Value, target reflect.Type) (reflect.Value, error) {
	if target == reflect.TypeFor[Value]() {
		return reflect.ValueOf(value), nil
	}
	switch target {
	case reflect.TypeFor[Date]():
		if value.kind != DateKind {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to monty.Date", value.kind)
		}
		return reflect.ValueOf(value.Date()), nil
	case reflect.TypeFor[DateTime]():
		if value.kind != DateTimeKind {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to monty.DateTime", value.kind)
		}
		return reflect.ValueOf(value.DateTime()), nil
	case reflect.TypeFor[TimeDelta]():
		if value.kind != TimeDeltaKind {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to monty.TimeDelta", value.kind)
		}
		return reflect.ValueOf(value.TimeDelta()), nil
	case reflect.TypeFor[TimeZone]():
		if value.kind != TimeZoneKind {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to monty.TimeZone", value.kind)
		}
		return reflect.ValueOf(value.TimeZone()), nil
	case reflect.TypeFor[Path]():
		if value.kind != PathKind && value.kind != StringKind {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to monty.Path", value.kind)
		}
		return reflect.ValueOf(Path(value.text)), nil
	case reflect.TypeFor[NamedTuple]():
		if value.kind != NamedTupleKind {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to monty.NamedTuple", value.kind)
		}
		return reflect.ValueOf(value.NamedTuple()), nil
	case reflect.TypeFor[DataclassValue]():
		if value.kind != DataclassKind {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to monty.DataclassValue", value.kind)
		}
		return reflect.ValueOf(value.Dataclass()), nil
	case reflect.TypeFor[Exception]():
		if value.kind != ExceptionKind {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to monty.Exception", value.kind)
		}
		return reflect.ValueOf(value.Exception()), nil
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
	case reflect.TypeFor[*big.Int]():
		switch value.kind {
		case IntKind:
			return reflect.ValueOf(big.NewInt(value.intValue)), nil
		case BigIntKind:
			parsed, ok := new(big.Int).SetString(value.text, 10)
			if !ok {
				return reflect.Value{}, fmt.Errorf("monty: invalid big int payload %q", value.text)
			}
			return reflect.ValueOf(parsed), nil
		default:
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to *big.Int", value.kind)
		}
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
			result.SetBytes(append([]byte(nil), value.bytes...))
			return result, nil
		}
		items, ok := sequenceItems(value)
		if !ok {
			return reflect.Value{}, fmt.Errorf("monty: cannot assign %s to slice", value.kind)
		}
		result = reflect.MakeSlice(target, len(items), len(items))
		for i := range items {
			converted, err := valueAsReflect(items[i], target.Elem())
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
		for i := range items {
			converted, err := valueAsReflect(items[i], target.Elem())
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
			// SetMapIndex panics if the key's dynamic type is not comparable
			// (e.g. a Python tuple key landing in a map[any]any). Reject it
			// with a clear error instead of crashing the caller.
			if !key.Comparable() {
				return reflect.Value{}, fmt.Errorf("monty: dict key of kind %s is not usable as a Go map key", value.pairs[i].Key.kind)
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
		info := taggedFieldsFor(target)
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

// --------------------------------------------------------------------------
// Struct field binding
// --------------------------------------------------------------------------

type taggedField struct {
	name      string
	index     []int
	fieldType reflect.Type
}

type taggedFieldInfo struct {
	fields      []taggedField
	nameToField map[string]taggedField
}

var taggedFieldCache sync.Map

func taggedFieldsFor(t reflect.Type) taggedFieldInfo {
	if t == nil {
		return taggedFieldInfo{}
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return taggedFieldInfo{}
	}
	if cached, ok := taggedFieldCache.Load(t); ok {
		return cached.(taggedFieldInfo) //nolint:errcheck // taggedFieldCache only stores taggedFieldInfo
	}
	fields := buildTaggedFields(t)
	nameToField := make(map[string]taggedField, len(fields))
	for _, field := range fields {
		nameToField[field.name] = field
	}
	info := taggedFieldInfo{fields: fields, nameToField: nameToField}
	actual, _ := taggedFieldCache.LoadOrStore(t, info)
	return actual.(taggedFieldInfo) //nolint:errcheck // taggedFieldCache only stores taggedFieldInfo
}

func buildTaggedFields(t reflect.Type) []taggedField {
	if t == nil {
		return nil
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	fields := make([]taggedField, 0, t.NumField())
	for field := range t.Fields() {
		if field.PkgPath != "" {
			continue
		}
		name, ok := fieldName(field)
		if ok {
			fields = append(fields, taggedField{
				name:      name,
				index:     append([]int(nil), field.Index...),
				fieldType: field.Type,
			})
		}
	}
	return fields
}

// --------------------------------------------------------------------------
// Input normalization
// --------------------------------------------------------------------------

// inputsToValues normalizes the `inputs any` argument accepted by Run, Start,
// Eval, and REPL methods into named values: nil, map[string]Value,
// map[string]any, or a struct/pointer-to-struct with monty tags.
func inputsToValues(inputs any) (map[string]Value, error) {
	switch v := inputs.(type) {
	case nil:
		return nil, nil
	case map[string]Value:
		return v, nil
	case map[string]any:
		converted := make(map[string]Value, len(v))
		for name, item := range v {
			value, err := From(item)
			if err != nil {
				return nil, fmt.Errorf("monty: input %q: %w", name, err)
			}
			converted[name] = value
		}
		return converted, nil
	}
	converted, err := From(inputs)
	if err != nil {
		return nil, err
	}
	if converted.kind != DictKind {
		return nil, fmt.Errorf("monty: inputs must be nil, map[string]Value, map[string]any, or a struct, got %T", inputs)
	}
	named := make(map[string]Value, len(converted.pairs))
	for i := range converted.pairs {
		pair := &converted.pairs[i]
		if pair.Key.kind != StringKind {
			return nil, fmt.Errorf("monty: input field key is %s, not string", pair.Key.kind)
		}
		named[pair.Key.text] = pair.Value
	}
	return named, nil
}
