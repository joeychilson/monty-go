package monty

import (
	"fmt"
	"reflect"
)

// As converts a Monty Value into a Go value of type T.
//
// The conversion accepts primitive Go types, structs with monty tags, maps,
// slices, interfaces, and Value itself.
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

func isStringLikeKind(kind Kind) bool {
	switch kind {
	case StringKind, BigIntKind, PathKind, ReprKind, CycleKind, FunctionKind, ExceptionKind, TypeKind, BuiltinFunctionKind:
		return true
	default:
		return false
	}
}
