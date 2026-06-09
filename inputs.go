package monty

import "fmt"

// Inputs maps Monty input names to Python values.
type Inputs map[string]Value

// InputsOf converts a Go struct or map into named Monty inputs.
//
// Struct field names are converted to snake_case by default. A `monty:"name"`
// tag overrides the Python input name, and `monty:"-"` skips a field.
//
// Conversion errors (an unsupported field type, an over-deep value, or a
// non-struct/non-map argument) are swallowed and surface as an empty Inputs.
// The run then fails far from the real cause with a "missing input" error — or,
// worse, succeeds with Python-side defaults. Prefer one of the recoverable
// forms: call InputsOfE to get the error, or pass the struct/map straight to
// Program.Run, Program.Start, RunAs, RunJSON, or CompileAndRun, all of which
// normalize the value themselves and report any conversion error.
func InputsOf(value any) Inputs {
	inputs, err := normalizeInputs(value)
	if err != nil {
		return Inputs{}
	}
	return inputs
}

// InputsOfE is the error-returning form of InputsOf. It reports conversion
// failures instead of swallowing them into an empty Inputs.
func InputsOfE(value any) (Inputs, error) {
	return normalizeInputs(value)
}

func normalizeInputs(value any) (Inputs, error) {
	switch v := value.(type) {
	case nil:
		return Inputs{}, nil
	case Inputs:
		return v, nil
	case map[string]Value:
		return Inputs(v), nil
	}
	converted, err := From(value)
	if err != nil {
		return nil, err
	}
	if converted.kind != DictKind {
		return nil, fmt.Errorf("monty: inputs must be monty.Inputs, map[string]Value, or a struct")
	}
	inputs := make(Inputs, len(converted.pairs))
	for i := range converted.pairs {
		pair := &converted.pairs[i]
		if pair.Key.kind != StringKind {
			return nil, fmt.Errorf("monty: input field key is %s, not string", pair.Key.kind)
		}
		inputs[pair.Key.text] = pair.Value
	}
	return inputs, nil
}
