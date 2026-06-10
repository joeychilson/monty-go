package ffi

import (
	"errors"
	"fmt"
)

// ValueFree releases a Rust-side value handle.
func ValueFree(handle uintptr) {
	if handle != 0 {
		mgValueFree(handle)
	}
}

// ValueJSON serializes a value handle to JSON bytes.
func ValueJSON(handle uintptr) ([]byte, error) {
	var buffer Bytes
	var errHandle uintptr
	status := mgValueJSON(handle, ptrOf(&buffer), ptrOf(&errHandle))
	if status != StatusOK {
		return nil, TakeError(errHandle)
	}
	return TakeBytes(buffer), nil
}

// ValueNone creates a Python None value handle.
func ValueNone() uintptr { return mgValueNone() }

// ValueEllipsis creates a Python Ellipsis value handle.
func ValueEllipsis() uintptr { return mgValueEllipsis() }

// ValueBool creates a Python bool value handle.
func ValueBool(v bool) uintptr { return mgValueBool(boolByte(v)) }

// ValueInt creates a Python int value handle from an int64.
func ValueInt(v int64) uintptr { return mgValueInt(v) }

// ValueFloat creates a Python float value handle.
func ValueFloat(v float64) uintptr { return mgValueFloat(v) }

// ValueKind returns the RawValue kind discriminant for a value handle.
func ValueKind(v uintptr) uint32 { return mgValueKind(v) }

// ValueBoolGet extracts a bool payload from a value handle.
func ValueBoolGet(v uintptr) bool { return mgValueBoolGet(v) != 0 }

// ValueIntGet extracts an int64 payload from a value handle.
func ValueIntGet(v uintptr) int64 { return mgValueIntGet(v) }

// ValueFloatGet extracts a float64 payload from a value handle.
func ValueFloatGet(v uintptr) float64 { return mgValueFloatGet(v) }

// ValueLen returns the item count for sequence and mapping value handles.
func ValueLen(v uintptr) uintptr { return mgValueLen(v) }

// ValueItemsRaw copies sequence items into out as RawValue values.
func ValueItemsRaw(v uintptr, out []RawValue) error {
	if status := mgValueItemsRaw(v, slicePointer(out), uintptr(len(out))); status != StatusOK {
		return errors.New("monty: value is not a sequence")
	}
	return nil
}

// ValuePairsRaw copies dict pairs into out as RawPair values.
func ValuePairsRaw(v uintptr, out []RawPair) error {
	if status := mgValuePairsRaw(v, slicePointer(out), uintptr(len(out))); status != StatusOK {
		return errors.New("monty: value is not a dict")
	}
	return nil
}

// ValueBigInt creates a Python int value handle from a base-10 string.
func ValueBigInt(v string) (uintptr, error) {
	var handle, errHandle uintptr
	valuePtr, valueLen := stringArgs(v)
	status := mgValueBigInt(valuePtr, valueLen, ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueString creates a Python str value handle.
func ValueString(v string) (uintptr, error) {
	var handle, errHandle uintptr
	valuePtr, valueLen := stringArgs(v)
	status := mgValueString(valuePtr, valueLen, ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValuePath creates a Python pathlib-style path value handle.
func ValuePath(v string) (uintptr, error) {
	var handle, errHandle uintptr
	valuePtr, valueLen := stringArgs(v)
	status := mgValuePath(valuePtr, valueLen, ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueBytes creates a Python bytes value handle.
func ValueBytes(v []byte) (uintptr, error) {
	valuePtr, valueLen := BytesRef(v)
	var handle, errHandle uintptr
	status := mgValueBytes(valuePtr, valueLen, ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueListRaw creates a Python list handle from raw values.
func ValueListRaw(values []RawValue) (uintptr, error) {
	var handle, errHandle uintptr
	status := mgValueListRaw(slicePointer(values), uintptr(len(values)), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueTupleRaw creates a Python tuple handle from raw values.
func ValueTupleRaw(values []RawValue) (uintptr, error) {
	var handle, errHandle uintptr
	status := mgValueTupleRaw(slicePointer(values), uintptr(len(values)), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueNamedTupleRaw creates a Python namedtuple-like handle from raw values.
func ValueNamedTupleRaw(typeName string, fieldNames []string, values []RawValue) (uintptr, error) {
	if len(fieldNames) != len(values) {
		return 0, errors.New("monty: named tuple field names and values have different lengths")
	}
	namePtr, nameLen := stringArgs(typeName)
	names := StringRefs(fieldNames)
	var handle, errHandle uintptr
	status := mgValueNamedTupleRaw(namePtr, nameLen, slicePointer(names), slicePointer(values), uintptr(len(values)), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueSetRaw creates a Python set handle from raw values.
func ValueSetRaw(values []RawValue) (uintptr, error) {
	var handle, errHandle uintptr
	status := mgValueSetRaw(slicePointer(values), uintptr(len(values)), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueFrozenSetRaw creates a Python frozenset handle from raw values.
func ValueFrozenSetRaw(values []RawValue) (uintptr, error) {
	var handle, errHandle uintptr
	status := mgValueFrozenSetRaw(slicePointer(values), uintptr(len(values)), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueDictRaw creates a Python dict handle from raw key/value pairs.
func ValueDictRaw(pairs []RawPair) (uintptr, error) {
	var handle, errHandle uintptr
	status := mgValueDictRaw(slicePointer(pairs), uintptr(len(pairs)), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueDate creates a Python date handle.
func ValueDate(value Date) (uintptr, error) {
	var handle, errHandle uintptr
	status := mgValueDate(ptrOf(&value), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueDateTime creates a Python datetime handle.
func ValueDateTime(value DateTime) (uintptr, error) {
	var handle, errHandle uintptr
	status := mgValueDateTime(ptrOf(&value), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueTimeDelta creates a Python timedelta handle.
func ValueTimeDelta(value TimeDelta) (uintptr, error) {
	var handle, errHandle uintptr
	status := mgValueTimeDelta(ptrOf(&value), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueTimeZone creates a Python timezone handle.
func ValueTimeZone(value TimeZone) (uintptr, error) {
	var handle, errHandle uintptr
	status := mgValueTimeZone(ptrOf(&value), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueDataclassRaw creates a Python dataclass-like handle from raw pairs.
func ValueDataclassRaw(name string, typeID uint64, fieldNames []string, attrs []RawPair, frozen bool) (uintptr, error) {
	names := StringRefs(fieldNames)
	var handle, errHandle uintptr
	args := DataclassRawArgs{
		Name:       StringRef(name),
		TypeID:     typeID,
		FieldNames: slicePointer(names),
		FieldCount: uintptr(len(names)),
		Attrs:      slicePointer(attrs),
		AttrCount:  uintptr(len(attrs)),
		Frozen:     boolByte(frozen),
	}
	status := mgValueDataclassRaw(ptrOf(&args), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueFunction creates a Python external-function marker handle.
func ValueFunction(name, doc string) (uintptr, error) {
	var handle, errHandle uintptr
	namePtr, nameLen := stringArgs(name)
	docPtr, docLen := stringArgs(doc)
	status := mgValueFunction(namePtr, nameLen, docPtr, docLen, ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueException creates a Python exception value handle.
func ValueException(excType, message string) (uintptr, error) {
	var handle, errHandle uintptr
	excPtr, excLen := stringArgs(excType)
	msgPtr, msgLen := stringArgs(message)
	status := mgValueException(excPtr, excLen, msgPtr, msgLen, ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ValueExceptionType returns the exception type name from a value handle.
func ValueExceptionType(v uintptr) (string, error) {
	var text Bytes
	var errHandle uintptr
	status := mgValueExceptionType(v, ptrOf(&text), ptrOf(&errHandle))
	if status != StatusOK {
		return "", TakeError(errHandle)
	}
	return TakeString(text), nil
}

// ValueExceptionMessage returns the exception message from a value handle.
func ValueExceptionMessage(v uintptr) (string, error) {
	var text Bytes
	var errHandle uintptr
	status := mgValueExceptionMessage(v, ptrOf(&text), ptrOf(&errHandle))
	if status != StatusOK {
		return "", TakeError(errHandle)
	}
	return TakeString(text), nil
}

// ValueText returns the textual payload or representation for a value handle.
func ValueText(v uintptr) (string, error) {
	var text Bytes
	var errHandle uintptr
	status := mgValueText(v, ptrOf(&text), ptrOf(&errHandle))
	if status != StatusOK {
		return "", TakeError(errHandle)
	}
	return TakeString(text), nil
}

// ValueBytesGet returns the byte payload from a bytes value handle.
func ValueBytesGet(v uintptr) ([]byte, error) {
	var bytes Bytes
	var errHandle uintptr
	status := mgValueBytesGet(v, ptrOf(&bytes), ptrOf(&errHandle))
	if status != StatusOK {
		return nil, TakeError(errHandle)
	}
	return TakeBytes(bytes), nil
}

// ValueDateGet returns the date payload from a value handle.
func ValueDateGet(v uintptr) (Date, error) {
	var value Date
	var errHandle uintptr
	status := mgValueDateGet(v, ptrOf(&value), ptrOf(&errHandle))
	if status != StatusOK {
		return Date{}, TakeError(errHandle)
	}
	return value, nil
}

// ValueDateTimeGet returns the datetime payload from a value handle.
func ValueDateTimeGet(v uintptr) (DateTime, error) {
	var value DateTime
	var errHandle uintptr
	status := mgValueDateTimeGet(v, ptrOf(&value), ptrOf(&errHandle))
	if status != StatusOK {
		return DateTime{}, TakeError(errHandle)
	}
	return value, nil
}

// ValueTimeDeltaGet returns the timedelta payload from a value handle.
func ValueTimeDeltaGet(v uintptr) (TimeDelta, error) {
	var value TimeDelta
	var errHandle uintptr
	status := mgValueTimeDeltaGet(v, ptrOf(&value), ptrOf(&errHandle))
	if status != StatusOK {
		return TimeDelta{}, TakeError(errHandle)
	}
	return value, nil
}

// ValueTimeZoneGet returns the timezone payload from a value handle.
func ValueTimeZoneGet(v uintptr) (TimeZone, error) {
	var value TimeZone
	var errHandle uintptr
	status := mgValueTimeZoneGet(v, ptrOf(&value), ptrOf(&errHandle))
	if status != StatusOK {
		return TimeZone{}, TakeError(errHandle)
	}
	return value, nil
}

// ValueNamedTupleTypeName returns the type name from a namedtuple value handle.
func ValueNamedTupleTypeName(v uintptr) (string, error) {
	var text Bytes
	var errHandle uintptr
	status := mgValueNamedTupleTypeName(v, ptrOf(&text), ptrOf(&errHandle))
	if status != StatusOK {
		return "", TakeError(errHandle)
	}
	return TakeString(text), nil
}

// ValueNamedTupleFieldNames returns field names from a namedtuple value handle.
func ValueNamedTupleFieldNames(v uintptr) ([]string, error) {
	var handle, errHandle uintptr
	status := mgValueNamedTupleFieldNames(v, ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return nil, TakeError(errHandle)
	}
	defer ValueFree(handle)
	return stringListFromValue(handle)
}

// ValueDataclassName returns the class name from a dataclass value handle.
func ValueDataclassName(v uintptr) (string, error) {
	var text Bytes
	var errHandle uintptr
	status := mgValueDataclassName(v, ptrOf(&text), ptrOf(&errHandle))
	if status != StatusOK {
		return "", TakeError(errHandle)
	}
	return TakeString(text), nil
}

// ValueDataclassTypeID returns the type ID from a dataclass value handle.
func ValueDataclassTypeID(v uintptr) uint64 { return mgValueDataclassTypeID(v) }

// ValueDataclassFieldNames returns field names from a dataclass value handle.
func ValueDataclassFieldNames(v uintptr) ([]string, error) {
	var handle, errHandle uintptr
	status := mgValueDataclassFieldNames(v, ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return nil, TakeError(errHandle)
	}
	defer ValueFree(handle)
	return stringListFromValue(handle)
}

// ValueDataclassFrozen reports whether a dataclass value handle is frozen.
func ValueDataclassFrozen(v uintptr) bool { return mgValueDataclassFrozen(v) != 0 }

func stringListFromValue(handle uintptr) ([]string, error) {
	count := int(ValueLen(handle))
	if count == 0 {
		return nil, nil
	}
	rawItems := make([]RawValue, count)
	if err := ValueItemsRaw(handle, rawItems); err != nil {
		return nil, err
	}
	names := make([]string, count)
	for i := range rawItems {
		if rawItems[i].Kind != KindString {
			for j := i; j < len(rawItems); j++ {
				RawValueFree(&rawItems[j])
			}
			return nil, fmt.Errorf("monty: field name %d is %d, not string", i, rawItems[i].Kind)
		}
		names[i] = TakeString(Bytes{Ptr: rawItems[i].Ptr, Len: rawItems[i].Len})
		rawItems[i].Ptr = nil
		rawItems[i].Len = 0
	}
	return names, nil
}
