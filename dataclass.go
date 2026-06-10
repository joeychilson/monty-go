package monty

import (
	"fmt"
	"hash/fnv"
	"reflect"
	"strings"
)

// DataclassType binds a Go struct type to a Python dataclass so values
// round-trip with class identity. Register bindings on Compile or NewREPL
// with WithDataclasses: the generated @dataclass stub joins type checking,
// and top-level inputs of the bound Go type encode as dataclass instances.
// Dataclass outputs decode into the Go type through As or Value.Decode.
type DataclassType struct {
	name   string
	goType reflect.Type
	fields []taggedField
	typeID uint64
	frozen bool
}

// DataclassOption configures DataclassFor.
type DataclassOption func(*DataclassType)

// WithFrozen marks the dataclass frozen (immutable inside Python).
func WithFrozen() DataclassOption {
	return func(d *DataclassType) { d.frozen = true }
}

// DataclassFor binds the struct type T to a Python dataclass named
// pythonName. Field names follow `monty` tags with snake_case defaults.
func DataclassFor[T any](pythonName string, opts ...DataclassOption) (*DataclassType, error) {
	goType := reflect.TypeFor[T]()
	base := goType
	if base.Kind() == reflect.Pointer {
		base = base.Elem()
	}
	if base.Kind() != reflect.Struct {
		return nil, fmt.Errorf("monty: DataclassFor requires a struct type, got %s", goType)
	}
	if pythonName == "" {
		return nil, fmt.Errorf("monty: DataclassFor requires a Python class name")
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(pythonName))
	dataclass := &DataclassType{
		name:   pythonName,
		goType: base,
		fields: taggedFieldsFor(base).fields,
		typeID: hash.Sum64(),
	}
	for _, opt := range opts {
		opt(dataclass)
	}
	return dataclass, nil
}

// Name returns the Python class name.
func (d *DataclassType) Name() string { return d.name }

// Stub returns the generated @dataclass stub used for type checking.
func (d *DataclassType) Stub() string {
	var b strings.Builder
	b.WriteString("from dataclasses import dataclass\n\n")
	if d.frozen {
		b.WriteString("@dataclass(frozen=True)\n")
	} else {
		b.WriteString("@dataclass\n")
	}
	fmt.Fprintf(&b, "class %s:\n", d.name)
	if len(d.fields) == 0 {
		b.WriteString("    pass")
		return b.String()
	}
	for i, field := range d.fields {
		fmt.Fprintf(&b, "    %s: %s", field.name, pythonType(field.fieldType))
		if i != len(d.fields)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// matches reports whether the dynamic Go type binds to this dataclass.
func (d *DataclassType) matches(t reflect.Type) bool {
	if t == nil {
		return false
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t == d.goType
}

// Wrap encodes a value of the bound Go type as a dataclass instance.
func (d *DataclassType) Wrap(value any) (Value, error) {
	reflected := reflect.ValueOf(value)
	if reflected.Kind() == reflect.Pointer {
		if reflected.IsNil() {
			return None(), nil
		}
		reflected = reflected.Elem()
	}
	if !reflected.IsValid() || reflected.Type() != d.goType {
		return Value{}, fmt.Errorf("monty: dataclass %s wraps %s, got %T", d.name, d.goType, value)
	}
	attrs := make([]Pair, 0, len(d.fields))
	fieldNames := make([]string, 0, len(d.fields))
	for _, field := range d.fields {
		converted, err := valueFromReflect(reflected.FieldByIndex(field.index), 0)
		if err != nil {
			return Value{}, fmt.Errorf("monty: dataclass %s field %s: %w", d.name, field.name, err)
		}
		attrs = append(attrs, Pair{Key: Str(field.name), Value: converted})
		fieldNames = append(fieldNames, field.name)
	}
	return DataclassValue{
		Name:   d.name,
		TypeID: d.typeID,
		Fields: fieldNames,
		Attrs:  attrs,
		Frozen: d.frozen,
	}.MontyValue(), nil
}
