package monty

import "fmt"

// Inputs maps Monty input names to Python values.
type Inputs map[string]Value

// InputsOf converts a Go struct or map into named Monty inputs.
//
// Struct field names are converted to snake_case by default. A `monty:"name"`
// tag overrides the Python input name, and `monty:"-"` skips a field. Errors
// during conversion are swallowed and surface as an empty Inputs; use
// normalizeInputs internally when error context is needed.
func InputsOf(value any) Inputs {
	inputs, err := normalizeInputs(value)
	if err != nil {
		return Inputs{}
	}
	return inputs
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
