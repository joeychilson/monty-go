package monty

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"iter"
	"math"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/joeychilson/monty/internal/ffi"
)

// Kind identifies the Python value variant stored in a Value.
type Kind uint32

const (
	// InvalidKind is the zero value kind and is not a valid Python value.
	InvalidKind Kind = ffi.KindInvalid
	// EllipsisKind is Python's Ellipsis singleton.
	EllipsisKind Kind = ffi.KindEllipsis
	// NoneKind is Python's None singleton.
	NoneKind Kind = ffi.KindNone
	// BoolKind is a Python bool.
	BoolKind Kind = ffi.KindBool
	// IntKind is a Python int representable as int64.
	IntKind Kind = ffi.KindInt
	// BigIntKind is a Python int larger than int64.
	BigIntKind Kind = ffi.KindBigInt
	// FloatKind is a Python float.
	FloatKind Kind = ffi.KindFloat
	// StringKind is a Python str.
	StringKind Kind = ffi.KindString
	// BytesKind is a Python bytes value.
	BytesKind Kind = ffi.KindBytes
	// ListKind is a Python list.
	ListKind Kind = ffi.KindList
	// TupleKind is a Python tuple.
	TupleKind Kind = ffi.KindTuple
	// NamedTupleKind is a Python named tuple.
	NamedTupleKind Kind = ffi.KindNamedTuple
	// DictKind is a Python dict.
	DictKind Kind = ffi.KindDict
	// SetKind is a Python set.
	SetKind Kind = ffi.KindSet
	// FrozenSetKind is a Python frozenset.
	FrozenSetKind Kind = ffi.KindFrozenSet
	// DateKind is a Python date.
	DateKind Kind = ffi.KindDate
	// DateTimeKind is a Python datetime.
	DateTimeKind Kind = ffi.KindDateTime
	// TimeDeltaKind is a Python timedelta.
	TimeDeltaKind Kind = ffi.KindTimeDelta
	// TimeZoneKind is a Python timezone.
	TimeZoneKind Kind = ffi.KindTimeZone
	// ExceptionKind is a Python exception value.
	ExceptionKind Kind = ffi.KindException
	// TypeKind is a Python type object.
	TypeKind Kind = ffi.KindType
	// BuiltinFunctionKind is a Python built-in function.
	BuiltinFunctionKind Kind = ffi.KindBuiltinFunction
	// PathKind is a pathlib path value.
	PathKind Kind = ffi.KindPath
	// DataclassKind is a Python dataclass instance.
	DataclassKind Kind = ffi.KindDataclass
	// FunctionKind is a Python function value.
	FunctionKind Kind = ffi.KindFunction
	// ReprKind is a fallback Python representation.
	ReprKind Kind = ffi.KindRepr
	// CycleKind is a value whose representation contains a cycle.
	CycleKind Kind = ffi.KindCycle
)

// Value is a Go representation of a Python value returned by or passed into
// Monty.
//
// Values are read-only views: accessors and iterators expose the underlying
// storage without defensive copies, and constructors borrow the slices they
// are given. Do not mutate a slice after wrapping it in a Value or one
// obtained from a Value accessor.
type Value struct {
	kind       Kind
	boolValue  bool
	intValue   int64
	floatValue float64
	text       string
	bytes      []byte
	items      []Value
	pairs      []Pair
	*valueExtra
}

type valueExtra struct {
	doc        string
	date       Date
	datetime   DateTime
	timedelta  TimeDelta
	timezone   TimeZone
	typeName   string
	typeID     uint64
	fieldNames []string
	frozen     bool
}

var emptyValueExtra valueExtra

// Pair is a key/value entry used to build dict-like Values.
type Pair struct {
	// Key is the dict key.
	Key Value
	// Value is the dict value.
	Value Value
}

// KV builds a string-keyed dict entry.
func KV(key string, value Value) Pair {
	return Pair{Key: Str(key), Value: value}
}

// String returns a stable display name for k.
func (k Kind) String() string {
	switch k {
	case InvalidKind:
		return "Invalid"
	case EllipsisKind:
		return "Ellipsis"
	case NoneKind:
		return "None"
	case BoolKind:
		return "Bool"
	case IntKind:
		return "Int"
	case BigIntKind:
		return "BigInt"
	case FloatKind:
		return "Float"
	case StringKind:
		return "String"
	case BytesKind:
		return "Bytes"
	case ListKind:
		return "List"
	case TupleKind:
		return "Tuple"
	case NamedTupleKind:
		return "NamedTuple"
	case DictKind:
		return "Dict"
	case SetKind:
		return "Set"
	case FrozenSetKind:
		return "FrozenSet"
	case DateKind:
		return "Date"
	case DateTimeKind:
		return "DateTime"
	case TimeDeltaKind:
		return "TimeDelta"
	case TimeZoneKind:
		return "TimeZone"
	case ExceptionKind:
		return "Exception"
	case TypeKind:
		return "Type"
	case BuiltinFunctionKind:
		return "BuiltinFunction"
	case PathKind:
		return "Path"
	case DataclassKind:
		return "Dataclass"
	case FunctionKind:
		return "Function"
	case ReprKind:
		return "Repr"
	case CycleKind:
		return "Cycle"
	default:
		return fmt.Sprintf("Kind(%d)", uint32(k))
	}
}

// None returns Python None.
func None() Value { return Value{kind: NoneKind} }

// Ellipsis returns Python Ellipsis.
func Ellipsis() Value { return Value{kind: EllipsisKind} }

// Bool returns a Python bool.
func Bool(v bool) Value { return Value{kind: BoolKind, boolValue: v} }

// Int returns a Python int from a Go int.
func Int(v int) Value { return Value{kind: IntKind, intValue: int64(v)} }

// Int64 returns a Python int from a Go int64.
func Int64(v int64) Value { return Value{kind: IntKind, intValue: v} }

// BigInt returns a Python int from a decimal integer string.
func BigInt(v string) Value { return Value{kind: BigIntKind, text: v} }

// Float returns a Python float.
func Float(v float64) Value { return Value{kind: FloatKind, floatValue: v} }

// Str returns a Python str.
func Str(v string) Value { return Value{kind: StringKind, text: v} }

// Bytes returns a Python bytes value. The slice is borrowed, not copied; do
// not mutate it afterwards.
func Bytes(v []byte) Value { return Value{kind: BytesKind, bytes: v} }

// List returns a Python list. The values are borrowed, not copied.
func List(v ...Value) Value { return Value{kind: ListKind, items: v} }

// Tuple returns a Python tuple. The values are borrowed, not copied.
func Tuple(v ...Value) Value { return Value{kind: TupleKind, items: v} }

// Set returns a Python set. The values are borrowed, not copied.
func Set(v ...Value) Value { return Value{kind: SetKind, items: v} }

// FrozenSet returns a Python frozenset. The values are borrowed, not copied.
func FrozenSet(v ...Value) Value { return Value{kind: FrozenSetKind, items: v} }

// Dict returns a Python dict. The pairs are borrowed, not copied.
func Dict(pairs ...Pair) Value { return Value{kind: DictKind, pairs: pairs} }

// StringDict returns a Python dict with string keys. Iteration order over the
// map is unspecified, so the resulting dict order is too.
func StringDict(values map[string]Value) Value {
	pairs := make([]Pair, 0, len(values))
	for key, value := range values {
		pairs = append(pairs, Pair{Key: Str(key), Value: value})
	}
	return Value{kind: DictKind, pairs: pairs}
}

// ExternalFunction returns a Python function placeholder, typically used to
// answer a NameLookup for a host-provided function.
func ExternalFunction(name string) Value {
	return Value{kind: FunctionKind, text: name}
}

func functionValue(name, doc string) Value {
	if doc == "" {
		return Value{kind: FunctionKind, text: name}
	}
	return Value{kind: FunctionKind, text: name, valueExtra: &valueExtra{doc: doc}}
}

func exceptionValue(excType, message string) Value {
	return Exception{Type: excType, Message: message}.MontyValue()
}

// Kind returns the Python value kind.
func (v Value) Kind() Kind { return v.kind }

// Bool returns the underlying bool, or false for other kinds.
func (v Value) Bool() bool { return v.boolValue }

// Int64 returns the underlying int64, or 0 for other kinds.
func (v Value) Int64() int64 { return v.intValue }

// Int returns the underlying int as a Go int, or 0 for other kinds.
func (v Value) Int() int { return int(v.intValue) }

// Float64 returns the underlying float64, or 0 for other kinds.
func (v Value) Float64() float64 { return v.floatValue }

// Str returns the textual payload: the string itself for str values, and the
// text form of string-like kinds (big ints, paths, reprs, function names).
func (v Value) Str() string { return v.text }

// Path returns the underlying pathlib path.
func (v Value) Path() Path { return Path(v.text) }

// Bytes returns the underlying byte slice as a read-only view (no copy).
func (v Value) Bytes() []byte { return v.bytes }

// Date returns the structured datetime.date payload.
func (v Value) Date() Date { return v.extraPtr().date }

// DateTime returns the structured datetime.datetime payload.
func (v Value) DateTime() DateTime { return v.extraPtr().datetime }

// TimeDelta returns the structured datetime.timedelta payload.
func (v Value) TimeDelta() TimeDelta { return v.extraPtr().timedelta }

// TimeZone returns the structured datetime.timezone payload.
func (v Value) TimeZone() TimeZone { return v.extraPtr().timezone }

// NamedTuple returns the namedtuple payload as a read-only view.
func (v Value) NamedTuple() NamedTuple {
	extra := v.extraPtr()
	return NamedTuple{
		Type:   extra.typeName,
		Fields: extra.fieldNames,
		Values: v.items,
	}
}

// Dataclass returns the dataclass payload as a read-only view.
func (v Value) Dataclass() DataclassValue {
	extra := v.extraPtr()
	return DataclassValue{
		Name:   extra.typeName,
		TypeID: extra.typeID,
		Fields: extra.fieldNames,
		Attrs:  v.pairs,
		Frozen: extra.frozen,
	}
}

// Exception returns the structured exception payload.
func (v Value) Exception() Exception {
	return Exception{Type: v.extraPtr().typeName, Message: v.text}
}

func (v Value) extraPtr() *valueExtra {
	if v.valueExtra != nil {
		return v.valueExtra
	}
	return &emptyValueExtra
}

// Len returns the element count for sequences, sets, namedtuples, dicts, and
// dataclasses, the byte length for str/bytes payloads, and 0 otherwise.
func (v Value) Len() int {
	switch v.kind {
	case ListKind, TupleKind, SetKind, FrozenSetKind, NamedTupleKind:
		return len(v.items)
	case DictKind, DataclassKind:
		return len(v.pairs)
	case StringKind:
		return len(v.text)
	case BytesKind:
		return len(v.bytes)
	default:
		return 0
	}
}

// Index returns the i-th element of a sequence, set, or namedtuple, or the
// zero Value when out of range or not a sequence.
func (v Value) Index(i int) Value {
	if i < 0 || i >= len(v.items) {
		return Value{}
	}
	return v.items[i]
}

// Get looks up a dict (or dataclass attribute) entry by key. The key is
// converted with the same rules as From; string keys take a fast path.
func (v Value) Get(key any) (Value, bool) {
	if v.kind != DictKind && v.kind != DataclassKind {
		return Value{}, false
	}
	if name, ok := key.(string); ok {
		for i := range v.pairs {
			pair := &v.pairs[i]
			if pair.Key.kind == StringKind && pair.Key.text == name {
				return pair.Value, true
			}
		}
		return Value{}, false
	}
	converted, err := From(key)
	if err != nil {
		return Value{}, false
	}
	for i := range v.pairs {
		if valueEqual(v.pairs[i].Key, converted) {
			return v.pairs[i].Value, true
		}
	}
	return Value{}, false
}

// Attr returns a namedtuple field or dataclass attribute by name.
func (v Value) Attr(name string) (Value, bool) {
	switch v.kind {
	case NamedTupleKind:
		for i, field := range v.extraPtr().fieldNames {
			if field == name && i < len(v.items) {
				return v.items[i], true
			}
		}
		return Value{}, false
	case DataclassKind:
		return v.Get(name)
	default:
		return Value{}, false
	}
}

// Elems iterates the elements of a list, tuple, set, frozenset, or
// namedtuple without copying. Yields nothing for other kinds.
func (v Value) Elems() iter.Seq[Value] {
	items := v.items
	return func(yield func(Value) bool) {
		for i := range items {
			if !yield(items[i]) {
				return
			}
		}
	}
}

// Entries iterates the key/value pairs of a dict or dataclass in insertion
// order without copying. Yields nothing for other kinds.
func (v Value) Entries() iter.Seq2[Value, Value] {
	pairs := v.pairs
	return func(yield func(Value, Value) bool) {
		for i := range pairs {
			if !yield(pairs[i].Key, pairs[i].Value) {
				return
			}
		}
	}
}

// valueEqual reports semantic equality for hashable-style keys: scalars,
// strings, bytes, and tuples thereof. Non-comparable kinds are never equal.
func valueEqual(a, b Value) bool {
	if a.kind != b.kind {
		// Python: True == 1, 1 == 1.0. Keep it strict by kind except the
		// int/float overlap, which round-trips often.
		if (a.kind == IntKind && b.kind == FloatKind) || (a.kind == FloatKind && b.kind == IntKind) {
			return a.asFloat() == b.asFloat()
		}
		return false
	}
	switch a.kind {
	case NoneKind, EllipsisKind:
		return true
	case BoolKind:
		return a.boolValue == b.boolValue
	case IntKind:
		return a.intValue == b.intValue
	case FloatKind:
		return a.floatValue == b.floatValue
	case StringKind, BigIntKind, PathKind:
		return a.text == b.text
	case BytesKind:
		return bytes.Equal(a.bytes, b.bytes)
	case TupleKind:
		if len(a.items) != len(b.items) {
			return false
		}
		for i := range a.items {
			if !valueEqual(a.items[i], b.items[i]) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func (v Value) asFloat() float64 {
	if v.kind == IntKind {
		return float64(v.intValue)
	}
	return v.floatValue
}

// String returns a Python-like representation of v.
func (v Value) String() string {
	switch v.kind {
	case NoneKind:
		return "None"
	case BoolKind:
		if v.boolValue {
			return "True"
		}
		return "False"
	case IntKind:
		return strconv.FormatInt(v.intValue, 10)
	case FloatKind:
		return strconv.FormatFloat(v.floatValue, 'g', -1, 64)
	case StringKind, BigIntKind, PathKind, ReprKind, CycleKind, FunctionKind, TypeKind, BuiltinFunctionKind:
		return v.text
	case ExceptionKind:
		extra := v.extraPtr()
		if extra.typeName == "" {
			return v.text
		}
		if v.text == "" {
			return extra.typeName
		}
		return extra.typeName + ": " + v.text
	case BytesKind:
		return fmt.Sprintf("%q", v.bytes)
	case ListKind:
		return "[" + strings.Join(valuesToStrings(v.items), ", ") + "]"
	case SetKind:
		if len(v.items) == 0 {
			return "set()"
		}
		return "{" + strings.Join(valuesToStrings(v.items), ", ") + "}"
	case FrozenSetKind:
		if len(v.items) == 0 {
			return "frozenset()"
		}
		return "frozenset({" + strings.Join(valuesToStrings(v.items), ", ") + "})"
	case TupleKind:
		return "(" + strings.Join(valuesToStrings(v.items), ", ") + ")"
	case NamedTupleKind:
		extra := v.extraPtr()
		return formatNamedTuple(extra.typeName, extra.fieldNames, v.items)
	case DictKind:
		parts := make([]string, 0, len(v.pairs))
		for pairIndex := range v.pairs {
			pair := &v.pairs[pairIndex]
			parts = append(parts, pair.Key.String()+": "+pair.Value.String())
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case DataclassKind:
		return formatDataclass(v.extraPtr().typeName, v.pairs)
	case DateKind:
		date := v.extraPtr().date
		return fmt.Sprintf("%04d-%02d-%02d", date.Year, date.Month, date.Day)
	case DateTimeKind:
		datetime := v.extraPtr().datetime
		// Only an aware datetime carries a timezone suffix; a naive datetime
		// prints without one rather than claiming a UTC ("Z") it never asserted.
		layout := "2006-01-02T15:04:05.999999"
		if datetime.TZ != nil {
			layout += "Z07:00"
		}
		return datetime.Time().Format(layout)
	case TimeDeltaKind:
		delta := v.extraPtr().timedelta
		return fmt.Sprintf("%dd %ds %dus", delta.Days, delta.Seconds, delta.Microseconds)
	case TimeZoneKind:
		timezone := v.extraPtr().timezone
		if timezone.Name != "" {
			return timezone.Name
		}
		offset := int(timezone.Offset / time.Second)
		sign := "+"
		if offset < 0 {
			sign = "-"
			offset = -offset
		}
		return fmt.Sprintf("UTC%s%02d:%02d", sign, offset/3600, (offset%3600)/60)
	default:
		return v.kind.String()
	}
}

func formatNamedTuple(typeName string, fieldNames []string, values []Value) string {
	parts := make([]string, len(values))
	for i := range values {
		if i < len(fieldNames) && fieldNames[i] != "" {
			parts[i] = fieldNames[i] + "=" + values[i].String()
		} else {
			parts[i] = values[i].String()
		}
	}
	if typeName == "" {
		typeName = "namedtuple"
	}
	return typeName + "(" + strings.Join(parts, ", ") + ")"
}

func formatDataclass(name string, attrs []Pair) string {
	parts := make([]string, len(attrs))
	for i := range attrs {
		pair := &attrs[i]
		key := pair.Key.text
		if pair.Key.kind != StringKind {
			key = pair.Key.String()
		}
		parts[i] = key + "=" + pair.Value.String()
	}
	if name == "" {
		name = "dataclass"
	}
	return name + "(" + strings.Join(parts, ", ") + ")"
}

func valuesToStrings(values []Value) []string {
	formatted := make([]string, len(values))
	for i := range values {
		formatted[i] = values[i].String()
	}
	return formatted
}

// Interface converts v into ordinary Go values: nil, bool, int64, float64,
// string, []byte, []any, map[any]any, and the package payload types.
func (v Value) Interface() any {
	switch v.kind {
	case NoneKind:
		return nil
	case BoolKind:
		return v.boolValue
	case IntKind:
		return v.intValue
	case FloatKind:
		return v.floatValue
	case StringKind, BigIntKind, ReprKind, CycleKind, FunctionKind, TypeKind, BuiltinFunctionKind:
		return v.text
	case PathKind:
		return Path(v.text)
	case ExceptionKind:
		return v.Exception()
	case BytesKind:
		return v.bytes
	case ListKind, TupleKind, SetKind, FrozenSetKind:
		items := make([]any, len(v.items))
		// Index rather than range to avoid copying each ~100-byte Value element.
		for i := range v.items {
			items[i] = v.items[i].Interface()
		}
		return items
	case NamedTupleKind:
		return v.NamedTuple()
	case DictKind:
		values := make(map[any]any, len(v.pairs))
		for pairIndex := range v.pairs {
			pair := &v.pairs[pairIndex]
			key := pair.Key.Interface()
			// A Go map key must be comparable. Python keys such as tuples
			// convert to []any (and namedtuple/dataclass keys to structs
			// holding slices), which would panic on insertion with "hash of
			// unhashable type". Fall back to the key's Python-like string form
			// for any non-comparable representation so this cannot panic.
			if t := reflect.TypeOf(key); t != nil && !t.Comparable() {
				key = pair.Key.String()
			}
			values[key] = pair.Value.Interface()
		}
		return values
	case DateKind:
		return v.Date()
	case DateTimeKind:
		return v.DateTime()
	case TimeDeltaKind:
		return v.TimeDelta()
	case TimeZoneKind:
		return v.TimeZone()
	case DataclassKind:
		return v.Dataclass()
	default:
		return nil
	}
}

// MarshalJSON returns Monty's natural JSON form for v.
//
// It intentionally mirrors upstream JsonMontyObject semantics: JSON-native
// values serialize directly, while Python-only values use tagged objects such
// as {"$tuple": [...]} or {"$bytes": [...]}.
func (v Value) MarshalJSON() ([]byte, error) {
	handle, err := valueToHandle(v)
	if err != nil {
		return nil, err
	}
	defer ffi.ValueFree(handle)
	json, err := ffi.ValueJSON(handle)
	return json, normalizeError(err)
}

// --------------------------------------------------------------------------
// FFI encode (Value -> RawValue / handle)
// --------------------------------------------------------------------------

type rawArena struct {
	rawValueSlices [][]ffi.RawValue
	rawPairSlices  [][]ffi.RawPair
	ownsHandles    bool
}

func valueToHandle(v Value) (uintptr, error) {
	if err := ffi.EnsureLoaded(); err != nil {
		return 0, err
	}
	switch v.kind {
	case InvalidKind:
		return 0, fmt.Errorf("monty: invalid zero Value; use monty.None() for Python None")
	case EllipsisKind:
		return ffi.ValueEllipsis(), nil
	case NoneKind:
		return ffi.ValueNone(), nil
	case BoolKind:
		return ffi.ValueBool(v.boolValue), nil
	case IntKind:
		return ffi.ValueInt(v.intValue), nil
	case BigIntKind:
		return ffi.ValueBigInt(v.text)
	case FloatKind:
		return ffi.ValueFloat(v.floatValue), nil
	case StringKind:
		return ffi.ValueString(v.text)
	case PathKind:
		return ffi.ValuePath(v.text)
	case BytesKind:
		return ffi.ValueBytes(v.bytes)
	case ListKind:
		return sequenceHandle(v.items, false)
	case TupleKind:
		return sequenceHandle(v.items, true)
	case NamedTupleKind:
		return namedTupleHandle(v)
	case SetKind:
		return setHandle(v.items, false)
	case FrozenSetKind:
		return setHandle(v.items, true)
	case DictKind:
		return dictHandle(v.pairs)
	case DateKind:
		return ffi.ValueDate(toFFIDate(v.Date()))
	case DateTimeKind:
		return ffi.ValueDateTime(toFFIDateTime(v.DateTime()))
	case TimeDeltaKind:
		return ffi.ValueTimeDelta(toFFITimeDelta(v.TimeDelta()))
	case TimeZoneKind:
		return ffi.ValueTimeZone(toFFITimeZone(v.TimeZone()))
	case DataclassKind:
		return dataclassHandle(v)
	case FunctionKind:
		return ffi.ValueFunction(v.text, v.extraPtr().doc)
	case ExceptionKind:
		excType := v.extraPtr().typeName
		if excType == "" {
			excType = "RuntimeError"
		}
		return ffi.ValueException(excType, v.text)
	default:
		return ffi.ValueString(v.text)
	}
}

func valueToRaw(v Value, arena *rawArena) (ffi.RawValue, error) {
	if raw, ok := tryBorrowRawValue(v); ok {
		return raw, nil
	}
	raw := ffi.RawValue{Kind: uint32(v.kind)}
	switch v.kind {
	case InvalidKind:
		return ffi.RawValue{}, fmt.Errorf("monty: invalid zero Value; use monty.None() for Python None")
	case EllipsisKind, NoneKind:
		return raw, nil
	case BoolKind:
		if v.boolValue {
			raw.Bool = 1
		}
		return raw, nil
	case IntKind:
		raw.Int = v.intValue
		return raw, nil
	case BigIntKind, StringKind:
		if v.text != "" {
			raw.Ptr = unsafe.Pointer(unsafe.StringData(v.text))
			raw.Len = uintptr(len(v.text))
		}
		return raw, nil
	case FunctionKind:
		if v.extraPtr().doc != "" {
			handle, err := valueToHandle(v)
			if err != nil {
				return ffi.RawValue{}, err
			}
			if arena != nil {
				arena.ownsHandles = true
			}
			return ownedRawHandle(handle), nil
		}
		if v.text != "" {
			raw.Ptr = unsafe.Pointer(unsafe.StringData(v.text))
			raw.Len = uintptr(len(v.text))
		}
		return raw, nil
	case FloatKind:
		raw.Float = v.floatValue
		return raw, nil
	case BytesKind:
		if len(v.bytes) != 0 {
			raw.Ptr = unsafe.Pointer(unsafe.SliceData(v.bytes))
			raw.Len = uintptr(len(v.bytes))
		}
		return raw, nil
	case ListKind, TupleKind, SetKind, FrozenSetKind:
		values, err := valuesToRaw(v.items, arena)
		if err != nil {
			return ffi.RawValue{}, err
		}
		if len(values) != 0 {
			raw.Ptr = unsafe.Pointer(unsafe.SliceData(values))
			raw.Len = uintptr(len(values))
		}
		return raw, nil
	case DictKind:
		pairs, err := pairsToRaw(v.pairs, arena)
		if err != nil {
			return ffi.RawValue{}, err
		}
		if len(pairs) != 0 {
			raw.Ptr = unsafe.Pointer(unsafe.SliceData(pairs))
			raw.Len = uintptr(len(pairs))
		}
		return raw, nil
	default:
		handle, err := valueToHandle(v)
		if err != nil {
			return ffi.RawValue{}, err
		}
		if arena != nil {
			arena.ownsHandles = true
		}
		return ownedRawHandle(handle), nil
	}
}

func ownedRawHandle(handle uintptr) ffi.RawValue {
	return ffi.RawValue{Kind: ffi.KindOwnedHandle, Handle: handle}
}

func tryBorrowRawValue(v Value) (ffi.RawValue, bool) {
	raw := ffi.RawValue{Kind: uint32(v.kind)}
	switch v.kind {
	case EllipsisKind, NoneKind:
		return raw, true
	case BoolKind:
		if v.boolValue {
			raw.Bool = 1
		}
		return raw, true
	case IntKind:
		raw.Int = v.intValue
		return raw, true
	case BigIntKind, StringKind:
		if v.text != "" {
			raw.Ptr = unsafe.Pointer(unsafe.StringData(v.text))
			raw.Len = uintptr(len(v.text))
		}
		return raw, true
	case FunctionKind:
		if v.extraPtr().doc != "" {
			return ffi.RawValue{}, false
		}
		if v.text != "" {
			raw.Ptr = unsafe.Pointer(unsafe.StringData(v.text))
			raw.Len = uintptr(len(v.text))
		}
		return raw, true
	case FloatKind:
		raw.Float = v.floatValue
		return raw, true
	case BytesKind:
		if len(v.bytes) != 0 {
			raw.Ptr = unsafe.Pointer(unsafe.SliceData(v.bytes))
			raw.Len = uintptr(len(v.bytes))
		}
		return raw, true
	case ListKind, TupleKind, SetKind, FrozenSetKind:
		// Empty containers borrow trivially (no item buffer); non-empty ones are
		// built on demand by valueToRaw via the arena.
		if len(v.items) == 0 {
			return raw, true
		}
		return ffi.RawValue{}, false
	case DictKind:
		if len(v.pairs) == 0 {
			return raw, true
		}
		return ffi.RawValue{}, false
	default:
		return ffi.RawValue{}, false
	}
}

func sequenceHandle(items []Value, asTuple bool) (uintptr, error) {
	arena := &rawArena{}
	values, err := valuesToRaw(items, arena)
	if err != nil {
		return 0, err
	}
	defer freeOwnedRawValues(values)
	var handle uintptr
	if asTuple {
		handle, err = ffi.ValueTupleRaw(values)
	} else {
		handle, err = ffi.ValueListRaw(values)
	}
	runtime.KeepAlive(arena)
	runtime.KeepAlive(items)
	return handle, err
}

func namedTupleHandle(v Value) (uintptr, error) {
	extra := v.extraPtr()
	if len(extra.fieldNames) != len(v.items) {
		return 0, fmt.Errorf("monty: named tuple has %d field names for %d values", len(extra.fieldNames), len(v.items))
	}
	arena := &rawArena{}
	values, err := valuesToRaw(v.items, arena)
	if err != nil {
		return 0, err
	}
	defer freeOwnedRawValues(values)
	handle, err := ffi.ValueNamedTupleRaw(extra.typeName, extra.fieldNames, values)
	runtime.KeepAlive(arena)
	runtime.KeepAlive(v)
	return handle, err
}

func setHandle(items []Value, asFrozenSet bool) (uintptr, error) {
	arena := &rawArena{}
	values, err := valuesToRaw(items, arena)
	if err != nil {
		return 0, err
	}
	defer freeOwnedRawValues(values)
	var handle uintptr
	if asFrozenSet {
		handle, err = ffi.ValueFrozenSetRaw(values)
	} else {
		handle, err = ffi.ValueSetRaw(values)
	}
	runtime.KeepAlive(arena)
	runtime.KeepAlive(items)
	return handle, err
}

func dictHandle(pairs []Pair) (uintptr, error) {
	arena := &rawArena{}
	rawPairs, err := pairsToRaw(pairs, arena)
	if err != nil {
		return 0, err
	}
	defer freeOwnedRawPairs(rawPairs)
	handle, err := ffi.ValueDictRaw(rawPairs)
	runtime.KeepAlive(arena)
	runtime.KeepAlive(pairs)
	return handle, err
}

func dataclassHandle(v Value) (uintptr, error) {
	extra := v.extraPtr()
	arena := &rawArena{}
	rawPairs, err := pairsToRaw(v.pairs, arena)
	if err != nil {
		return 0, err
	}
	defer freeOwnedRawPairs(rawPairs)
	handle, err := ffi.ValueDataclassRaw(extra.typeName, extra.typeID, extra.fieldNames, rawPairs, extra.frozen)
	runtime.KeepAlive(arena)
	runtime.KeepAlive(v)
	return handle, err
}

func toFFIDate(date Date) ffi.Date {
	//nolint:gosec // Python date fields are bounded: year fits int32, month/day fit uint8
	return ffi.Date{Year: int32(date.Year), Month: uint8(date.Month), Day: uint8(date.Day)}
}

func toFFIDateTime(value DateTime) ffi.DateTime {
	//nolint:gosec // Python datetime fields are bounded by their calendar/clock ranges
	raw := ffi.DateTime{
		Year:        int32(value.Year),
		Microsecond: uint32(value.Microsecond),
		Month:       uint8(value.Month),
		Day:         uint8(value.Day),
		Hour:        uint8(value.Hour),
		Minute:      uint8(value.Minute),
		Second:      uint8(value.Second),
	}
	if value.TZ != nil {
		raw.HasOffset = 1
		//nolint:gosec // timezone offsets are bounded to ±24h in seconds; fits int32
		raw.OffsetSeconds = int32(value.TZ.Offset / time.Second)
		if value.TZ.Name != "" {
			raw.HasTimezone = 1
			raw.TimezoneName = ffi.Bytes{Ptr: unsafe.Pointer(unsafe.StringData(value.TZ.Name)), Len: uintptr(len(value.TZ.Name))}
		}
	}
	return raw
}

func toFFITimeDelta(value TimeDelta) ffi.TimeDelta {
	//nolint:gosec // Python timedelta fields fit int32 (days bounded to ~±1e9 by Python)
	return ffi.TimeDelta{
		Days:         int32(value.Days),
		Seconds:      int32(value.Seconds),
		Microseconds: int32(value.Microseconds),
	}
}

func toFFITimeZone(value TimeZone) ffi.TimeZone {
	//nolint:gosec // timezone offset bounded to ±14h in seconds; fits int32
	raw := ffi.TimeZone{OffsetSeconds: int32(value.Offset / time.Second)}
	if value.Name != "" {
		raw.HasName = 1
		raw.Name = ffi.Bytes{Ptr: unsafe.Pointer(unsafe.StringData(value.Name)), Len: uintptr(len(value.Name))}
	}
	return raw
}

func fromFFIDate(date ffi.Date) Date {
	return Date{Year: int(date.Year), Month: time.Month(date.Month), Day: int(date.Day)}
}

func fromFFIDateTime(value ffi.DateTime) DateTime {
	dt := DateTime{
		Year:        int(value.Year),
		Month:       time.Month(value.Month),
		Day:         int(value.Day),
		Hour:        int(value.Hour),
		Minute:      int(value.Minute),
		Second:      int(value.Second),
		Microsecond: int(value.Microsecond),
	}
	name := ffi.TakeString(value.TimezoneName)
	if value.HasOffset != 0 || value.HasTimezone != 0 {
		dt.TZ = &TimeZone{
			Offset: time.Duration(value.OffsetSeconds) * time.Second,
			Name:   name,
		}
	}
	return dt
}

func fromFFITimeDelta(value ffi.TimeDelta) TimeDelta {
	return TimeDelta{
		Days:         int(value.Days),
		Seconds:      int(value.Seconds),
		Microseconds: int(value.Microseconds),
	}
}

func fromFFITimeZone(value ffi.TimeZone) TimeZone {
	return TimeZone{
		Offset: time.Duration(value.OffsetSeconds) * time.Second,
		Name:   ffi.TakeString(value.Name),
	}
}

func valuesToRaw(items []Value, arena *rawArena) ([]ffi.RawValue, error) {
	rawItems := make([]ffi.RawValue, len(items))
	if arena != nil && len(rawItems) != 0 {
		arena.rawValueSlices = append(arena.rawValueSlices, rawItems)
	}
	for i, item := range items {
		raw, err := valueToRaw(item, arena)
		if err != nil {
			freeOwnedRawValues(rawItems)
			return nil, err
		}
		rawItems[i] = raw
	}
	return rawItems, nil
}

func pairsToRaw(pairs []Pair, arena *rawArena) ([]ffi.RawPair, error) {
	rawPairItems := make([]ffi.RawPair, len(pairs))
	if arena != nil && len(rawPairItems) != 0 {
		arena.rawPairSlices = append(arena.rawPairSlices, rawPairItems)
	}
	for i := range pairs {
		key, err := valueToRaw(pairs[i].Key, arena)
		if err != nil {
			freeOwnedRawPairs(rawPairItems)
			return nil, err
		}
		rawPairItems[i].Key = key
		value, err := valueToRaw(pairs[i].Value, arena)
		if err != nil {
			freeOwnedRawPairs(rawPairItems)
			return nil, err
		}
		rawPairItems[i].Value = value
	}
	return rawPairItems, nil
}

func freeOwnedRawValue(value *ffi.RawValue) {
	if value == nil {
		return
	}
	switch Kind(value.Kind) {
	case ListKind, TupleKind, SetKind, FrozenSetKind:
		if value.Ptr != nil && value.Len != 0 {
			values := unsafe.Slice((*ffi.RawValue)(value.Ptr), value.Len)
			freeOwnedRawValues(values)
		}
	case DictKind:
		if value.Ptr != nil && value.Len != 0 {
			pairs := unsafe.Slice((*ffi.RawPair)(value.Ptr), value.Len)
			freeOwnedRawPairs(pairs)
		}
	default:
		// scalar kinds have no owned child buffers to free
	}
	if value.Kind == ffi.KindOwnedHandle && value.Handle != 0 {
		ffi.ValueFree(value.Handle)
	}
	// Zero unconditionally so a repeat call is a no-op, mirroring the Rust
	// free_raw_value which resets to KIND_INVALID. The raw slices reaching here
	// are built fresh per call by valueToRaw, so clearing a container slot
	// cannot corrupt a value's state.
	*value = ffi.RawValue{}
}

func freeOwnedRawValues(values []ffi.RawValue) {
	for i := range values {
		freeOwnedRawValue(&values[i])
	}
}

func freeOwnedRawPairs(pairs []ffi.RawPair) {
	for i := range pairs {
		freeOwnedRawValue(&pairs[i].Key)
		freeOwnedRawValue(&pairs[i].Value)
	}
}

// --------------------------------------------------------------------------
// FFI decode (handle / RawValue / flat bytes -> Value)
// --------------------------------------------------------------------------

func decodeValue(handle uintptr) (Value, error) {
	if handle == 0 {
		return Value{}, fmt.Errorf("monty: null value handle")
	}
	kind := Kind(ffi.ValueKind(handle))
	switch kind {
	case EllipsisKind:
		return Ellipsis(), nil
	case NoneKind:
		return None(), nil
	case BoolKind:
		return Bool(ffi.ValueBoolGet(handle)), nil
	case IntKind:
		return Int64(ffi.ValueIntGet(handle)), nil
	case FloatKind:
		return Float(ffi.ValueFloatGet(handle)), nil
	case StringKind:
		text, err := ffi.ValueText(handle)
		return Value{kind: StringKind, text: text}, err
	case BigIntKind, PathKind, ReprKind, CycleKind, FunctionKind, TypeKind, BuiltinFunctionKind:
		text, err := ffi.ValueText(handle)
		return Value{kind: kind, text: text}, err
	case ExceptionKind:
		excType, err := ffi.ValueExceptionType(handle)
		if err != nil {
			return Value{}, normalizeError(err)
		}
		message, err := ffi.ValueExceptionMessage(handle)
		if err != nil {
			return Value{}, normalizeError(err)
		}
		return exceptionValue(excType, message), nil
	case BytesKind:
		data, err := ffi.ValueBytesGet(handle)
		return Value{kind: BytesKind, bytes: data}, err
	case ListKind, TupleKind, SetKind, FrozenSetKind:
		return decodeSequence(handle, kind)
	case NamedTupleKind:
		return decodeNamedTuple(handle)
	case DictKind:
		return decodeDict(handle, kind)
	case DateKind:
		date, err := ffi.ValueDateGet(handle)
		return fromFFIDate(date).MontyValue(), err
	case DateTimeKind:
		datetime, err := ffi.ValueDateTimeGet(handle)
		return fromFFIDateTime(datetime).MontyValue(), err
	case TimeDeltaKind:
		delta, err := ffi.ValueTimeDeltaGet(handle)
		return fromFFITimeDelta(delta).MontyValue(), err
	case TimeZoneKind:
		tz, err := ffi.ValueTimeZoneGet(handle)
		return fromFFITimeZone(tz).MontyValue(), err
	case DataclassKind:
		return decodeDataclass(handle)
	default:
		text, err := ffi.ValueText(handle)
		return Value{kind: ReprKind, text: text}, err
	}
}

func decodeSequence(handle uintptr, kind Kind) (Value, error) {
	itemCount := int(ffi.ValueLen(handle))
	if itemCount == 0 {
		return Value{kind: kind, items: nil}, nil
	}
	rawItems := make([]ffi.RawValue, itemCount)
	if err := ffi.ValueItemsRaw(handle, rawItems); err != nil {
		return Value{}, normalizeError(err)
	}
	items := make([]Value, itemCount)
	for i := range rawItems {
		item, err := decodeRawValueIntern(&rawItems[i], nil)
		if err != nil {
			// decodeRawValueIntern consumed and zeroed rawItems[i]; free the rest.
			freeAllRawValues(rawItems[i+1:])
			return Value{}, err
		}
		items[i] = item
	}
	return Value{kind: kind, items: items}, nil
}

func decodeNamedTuple(handle uintptr) (Value, error) {
	value, err := decodeSequence(handle, NamedTupleKind)
	if err != nil {
		return Value{}, err
	}
	typeName, err := ffi.ValueNamedTupleTypeName(handle)
	if err != nil {
		return Value{}, normalizeError(err)
	}
	fieldNames, err := ffi.ValueNamedTupleFieldNames(handle)
	if err != nil {
		return Value{}, normalizeError(err)
	}
	value.valueExtra = &valueExtra{typeName: typeName, fieldNames: fieldNames}
	return value, nil
}

func decodeDict(handle uintptr, kind Kind) (Value, error) {
	pairCount := int(ffi.ValueLen(handle))
	if pairCount == 0 {
		return Value{kind: kind, pairs: nil}, nil
	}
	rawPairs := make([]ffi.RawPair, pairCount)
	if err := ffi.ValuePairsRaw(handle, rawPairs); err != nil {
		return Value{}, normalizeError(err)
	}
	pairs := make([]Pair, pairCount)
	for i := range rawPairs {
		key, err := decodeRawValueIntern(&rawPairs[i].Key, nil)
		if err != nil {
			// Key consumed and zeroed; its value slot is still owned, as are
			// the remaining pairs.
			ffi.RawValueFree(&rawPairs[i].Value)
			freeAllRawPairs(rawPairs[i+1:])
			return Value{}, err
		}
		value, err := decodeRawValueIntern(&rawPairs[i].Value, nil)
		if err != nil {
			// Both slots of pair i are now consumed; free the remaining pairs.
			freeAllRawPairs(rawPairs[i+1:])
			return Value{}, err
		}
		pairs[i] = Pair{Key: key, Value: value}
	}
	return Value{kind: kind, pairs: pairs}, nil
}

func decodeDataclass(handle uintptr) (Value, error) {
	value, err := decodeDict(handle, DataclassKind)
	if err != nil {
		return Value{}, err
	}
	name, err := ffi.ValueDataclassName(handle)
	if err != nil {
		return Value{}, normalizeError(err)
	}
	fieldNames, err := ffi.ValueDataclassFieldNames(handle)
	if err != nil {
		return Value{}, normalizeError(err)
	}
	value.valueExtra = &valueExtra{
		typeName:   name,
		typeID:     ffi.ValueDataclassTypeID(handle),
		fieldNames: fieldNames,
		frozen:     ffi.ValueDataclassFrozen(handle),
	}
	return value, nil
}

func decodeOwnedValue(handle uintptr) (Value, error) {
	defer ffi.ValueFree(handle)
	return decodeValue(handle)
}

func decodeRawValue(raw ffi.RawValue) (Value, error) {
	return decodeRawValueIntern(&raw, nil)
}

// decodeRawValueIntern decodes one raw value, consuming it: on every exit path,
// success or failure, it releases whatever *raw owns (string/bytes buffers,
// child arrays, owned handles) and zeros *raw. Callers iterating an array of
// raw values therefore never need to free a slot they have already passed here,
// and a parent RawValueFree only ever walks already-zeroed slots for processed
// items — which is what prevents the partial-failure double-free when a nested
// container decode errors midway.
func decodeRawValueIntern(raw *ffi.RawValue, stringCache map[string]string) (Value, error) {
	kind := Kind(raw.Kind)
	switch kind {
	case InvalidKind:
		*raw = ffi.RawValue{}
		return Value{}, fmt.Errorf("monty: invalid raw value")
	case EllipsisKind:
		*raw = ffi.RawValue{}
		return Ellipsis(), nil
	case NoneKind:
		*raw = ffi.RawValue{}
		return None(), nil
	case BoolKind:
		b := raw.Bool != 0
		*raw = ffi.RawValue{}
		return Bool(b), nil
	case IntKind:
		n := raw.Int
		*raw = ffi.RawValue{}
		return Int64(n), nil
	case FloatKind:
		f := raw.Float
		*raw = ffi.RawValue{}
		return Float(f), nil
	case StringKind:
		text := ffi.TakeString(ffi.Bytes{Ptr: raw.Ptr, Len: raw.Len})
		*raw = ffi.RawValue{}
		return Value{kind: StringKind, text: text}, nil
	case BigIntKind, PathKind, ReprKind, CycleKind, FunctionKind, TypeKind, BuiltinFunctionKind:
		text := ffi.TakeString(ffi.Bytes{Ptr: raw.Ptr, Len: raw.Len})
		*raw = ffi.RawValue{}
		return Value{kind: kind, text: text}, nil
	case ExceptionKind:
		text := ffi.TakeString(ffi.Bytes{Ptr: raw.Ptr, Len: raw.Len})
		*raw = ffi.RawValue{}
		if excType, message, ok := strings.Cut(text, ": "); ok && excType != "" {
			return exceptionValue(excType, message), nil
		}
		return exceptionValue(text, ""), nil
	case BytesKind:
		data := ffi.TakeBytes(ffi.Bytes{Ptr: raw.Ptr, Len: raw.Len})
		*raw = ffi.RawValue{}
		return Value{kind: BytesKind, bytes: data}, nil
	case ListKind, TupleKind, SetKind, FrozenSetKind:
		if raw.Handle != 0 {
			handle := raw.Handle
			*raw = ffi.RawValue{}
			return decodeOwnedValue(handle)
		}
		return decodeRawSequence(raw, kind, stringCache)
	case DictKind:
		if raw.Handle != 0 {
			handle := raw.Handle
			*raw = ffi.RawValue{}
			return decodeOwnedValue(handle)
		}
		return decodeRawDict(raw, kind, stringCache)
	default:
		if raw.Handle == 0 {
			// Unknown kind with no handle (binding/library version skew).
			// Free any buffer it carries before reporting the error.
			ffi.RawValueFree(raw)
			return Value{}, fmt.Errorf("monty: raw %s value did not include a value handle", kind)
		}
		handle := raw.Handle
		*raw = ffi.RawValue{}
		return decodeOwnedValue(handle)
	}
}

func decodeRawSequence(raw *ffi.RawValue, kind Kind, stringCache map[string]string) (Value, error) {
	itemCount := int(raw.Len)
	if itemCount == 0 {
		ffi.RawValueFree(raw)
		return Value{kind: kind, items: nil}, nil
	}
	if raw.Ptr == nil {
		ffi.RawValueFree(raw)
		return Value{}, fmt.Errorf("monty: raw %s value has null item pointer", kind)
	}
	if stringCache == nil {
		stringCache = make(map[string]string)
	}
	rawItems := unsafe.Slice((*ffi.RawValue)(raw.Ptr), itemCount)
	items := make([]Value, itemCount)
	for i := range rawItems {
		item, err := decodeRawValueIntern(&rawItems[i], stringCache)
		if err != nil {
			// rawItems[i] is already consumed and zeroed; RawValueFree(raw)
			// frees the backing array plus the still-owned slots after i.
			ffi.RawValueFree(raw)
			return Value{}, err
		}
		items[i] = item
	}
	ffi.RawValueFree(raw)
	return Value{kind: kind, items: items}, nil
}

func decodeRawDict(raw *ffi.RawValue, kind Kind, stringCache map[string]string) (Value, error) {
	pairCount := int(raw.Len)
	if pairCount == 0 {
		ffi.RawValueFree(raw)
		return Value{kind: kind, pairs: nil}, nil
	}
	if raw.Ptr == nil {
		ffi.RawValueFree(raw)
		return Value{}, fmt.Errorf("monty: raw %s value has null pair pointer", kind)
	}
	if stringCache == nil {
		stringCache = make(map[string]string)
	}
	rawPairs := unsafe.Slice((*ffi.RawPair)(raw.Ptr), pairCount)
	pairs := make([]Pair, pairCount)
	for i := range rawPairs {
		var key Value
		var err error
		if Kind(rawPairs[i].Key.Kind) == StringKind {
			// takeInternedRawString consumes and zeros the key slot.
			key = Value{kind: StringKind, text: takeInternedRawString(&rawPairs[i].Key, stringCache)}
		} else {
			key, err = decodeRawValueIntern(&rawPairs[i].Key, stringCache)
			if err != nil {
				ffi.RawValueFree(raw)
				return Value{}, err
			}
		}
		value, err := decodeRawValueIntern(&rawPairs[i].Value, stringCache)
		if err != nil {
			ffi.RawValueFree(raw)
			return Value{}, err
		}
		pairs[i] = Pair{Key: key, Value: value}
	}
	ffi.RawValueFree(raw)
	return Value{kind: kind, pairs: pairs}, nil
}

func takeInternedRawString(raw *ffi.RawValue, cache map[string]string) string {
	if raw == nil || raw.Ptr == nil {
		if raw != nil {
			*raw = ffi.RawValue{}
		}
		return ""
	}
	data := unsafe.Slice((*byte)(raw.Ptr), int(raw.Len))
	probe := unsafe.String(unsafe.SliceData(data), len(data))
	if interned, ok := cache[probe]; ok {
		ffi.RawValueFree(raw)
		return interned
	}
	value := ffi.TakeString(ffi.Bytes{Ptr: raw.Ptr, Len: raw.Len})
	*raw = ffi.RawValue{}
	cache[value] = value
	return value
}

type flatValueReader struct {
	data        []byte
	offset      int
	copyStrings bool
}

// flatStringCopyThreshold bounds how large the flat result buffer may grow
// before decoded strings are copied instead of borrowed. Borrowing makes every
// string alias the single result buffer, so retaining one small string from a
// large result would pin the whole buffer; above this size the copy cost is
// worth releasing the buffer.
const flatStringCopyThreshold = 4 << 10

func decodeFlatValue(data []byte) (Value, error) {
	reader := flatValueReader{data: data, copyStrings: len(data) > flatStringCopyThreshold}
	value, err := reader.readValue()
	if err != nil {
		return Value{}, err
	}
	if reader.offset != len(data) {
		return Value{}, fmt.Errorf("monty: trailing flat value data")
	}
	return value, nil
}

func (r *flatValueReader) readValue() (Value, error) {
	kindRaw, err := r.readUint32()
	if err != nil {
		return Value{}, err
	}
	kind := Kind(kindRaw)
	switch kind {
	case EllipsisKind:
		return Ellipsis(), nil
	case NoneKind:
		return None(), nil
	case BoolKind:
		value, err := r.readByte()
		if err != nil {
			return Value{}, err
		}
		return Bool(value != 0), nil
	case IntKind:
		value, err := r.readInt64()
		if err != nil {
			return Value{}, err
		}
		return Int64(value), nil
	case BigIntKind:
		value, err := r.readString()
		if err != nil {
			return Value{}, err
		}
		return Value{kind: BigIntKind, text: value}, nil
	case FloatKind:
		value, err := r.readFloat64()
		if err != nil {
			return Value{}, err
		}
		return Float(value), nil
	case StringKind:
		value, err := r.readString()
		if err != nil {
			return Value{}, err
		}
		return Str(value), nil
	case BytesKind:
		value, err := r.readBytes()
		if err != nil {
			return Value{}, err
		}
		return Value{kind: BytesKind, bytes: append([]byte(nil), value...)}, nil
	case ListKind, TupleKind, SetKind, FrozenSetKind:
		count, err := r.readUint32()
		if err != nil {
			return Value{}, err
		}
		items := make([]Value, int(count))
		for i := range items {
			item, err := r.readValue()
			if err != nil {
				return Value{}, err
			}
			items[i] = item
		}
		return Value{kind: kind, items: items}, nil
	case DictKind:
		count, err := r.readUint32()
		if err != nil {
			return Value{}, err
		}
		pairs := make([]Pair, int(count))
		for i := range pairs {
			key, err := r.readValue()
			if err != nil {
				return Value{}, err
			}
			value, err := r.readValue()
			if err != nil {
				return Value{}, err
			}
			pairs[i] = Pair{Key: key, Value: value}
		}
		return Value{kind: DictKind, pairs: pairs}, nil
	default:
		return Value{}, fmt.Errorf("monty: unsupported flat value kind %s", kind)
	}
}

func (r *flatValueReader) readByte() (byte, error) {
	if r.offset >= len(r.data) {
		return 0, fmt.Errorf("monty: truncated flat value")
	}
	value := r.data[r.offset]
	r.offset++
	return value, nil
}

func (r *flatValueReader) readUint32() (uint32, error) {
	if len(r.data)-r.offset < 4 {
		return 0, fmt.Errorf("monty: truncated flat value")
	}
	value := binary.LittleEndian.Uint32(r.data[r.offset:])
	r.offset += 4
	return value, nil
}

func (r *flatValueReader) readUint64() (uint64, error) {
	if len(r.data)-r.offset < 8 {
		return 0, fmt.Errorf("monty: truncated flat value")
	}
	value := binary.LittleEndian.Uint64(r.data[r.offset:])
	r.offset += 8
	return value, nil
}

func (r *flatValueReader) readInt64() (int64, error) {
	value, err := r.readUint64()
	//nolint:gosec // intentional bit-pattern reinterpretation of an int64 stored as uint64
	return int64(value), err
}

func (r *flatValueReader) readFloat64() (float64, error) {
	bits, err := r.readUint64()
	if err != nil {
		return 0, err
	}
	return math.Float64frombits(bits), nil
}

func (r *flatValueReader) readBytes() ([]byte, error) {
	length, err := r.readUint32()
	if err != nil {
		return nil, err
	}
	end := r.offset + int(length)
	if end > len(r.data) {
		return nil, fmt.Errorf("monty: truncated flat value")
	}
	value := r.data[r.offset:end]
	r.offset = end
	return value, nil
}

func (r *flatValueReader) readString() (string, error) {
	data, err := r.readBytes()
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", nil
	}
	if r.copyStrings {
		// Large result buffer: copy so no returned string pins the whole buffer.
		return string(data), nil
	}
	// Small result buffer: the flat byte stream lives in a caller-owned buffer
	// that outlives the returned Value tree, so every string borrows its backing
	// directly via unsafe.String — no per-string allocation required. Repeated
	// keys keep separate headers but share underlying bytes, which is enough for
	// the public Value contract.
	return unsafe.String(unsafe.SliceData(data), len(data)), nil
}

func freeAllRawValues(values []ffi.RawValue) {
	for i := range values {
		ffi.RawValueFree(&values[i])
	}
}

func freeAllRawPairs(pairs []ffi.RawPair) {
	for i := range pairs {
		ffi.RawValueFree(&pairs[i].Key)
		ffi.RawValueFree(&pairs[i].Value)
	}
}
