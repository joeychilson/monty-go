package monty

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestFunctionSignatures(t *testing.T) {
	ctx := context.Background()

	t.Run("struct kwargs", func(t *testing.T) {
		type args struct {
			Width  int
			Height int
		}
		area := MustFunction("area", func(a args) int { return a.Width * a.Height })
		got, err := EvalAs[int](ctx, "area(3, height=4)", nil, WithFunctions(area))
		if err != nil || got != 12 {
			t.Fatalf("got %d, %v", got, err)
		}
	})

	t.Run("positional multi-param", func(t *testing.T) {
		join := MustFunction("join2", func(a, b string) string { return a + "|" + b })
		got, err := EvalAs[string](ctx, `join2("x", "y")`, nil, WithFunctions(join))
		if err != nil || got != "x|y" {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("with params kwargs", func(t *testing.T) {
		pad := MustFunction("pad", func(text string, width int) string {
			for len(text) < width {
				text += "."
			}
			return text
		}, WithParams("text", "width"))
		got, err := EvalAs[string](ctx, `pad("ab", width=4)`, nil, WithFunctions(pad))
		if err != nil || got != "ab.." {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("context and error", func(t *testing.T) {
		fail := MustFunction("fail", func(_ context.Context, n int) (int, error) {
			return 0, Errorf("KeyError", "no %d", n)
		})
		_, err := Eval(ctx, "fail(5)", nil, WithFunctions(fail))
		var execErr *ExecError
		if !errors.As(err, &execErr) || execErr.Type != "KeyError" {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("error only return", func(t *testing.T) {
		check := MustFunction("check", func(flag bool) error {
			if !flag {
				return Errorf("ValueError", "flag required")
			}
			return nil
		})
		value, err := Eval(ctx, "check(True)", nil, WithFunctions(check))
		if err != nil || value.Kind() != NoneKind {
			t.Fatalf("value %v err %v", value, err)
		}
	})

	t.Run("no params", func(t *testing.T) {
		fortytwo := MustFunction("fortytwo", func() int { return 42 })
		got, err := EvalAs[int](ctx, "fortytwo()", nil, WithFunctions(fortytwo))
		if err != nil || got != 42 {
			t.Fatalf("got %d, %v", got, err)
		}
	})

	t.Run("raw function", func(t *testing.T) {
		echo := NewRawFunction("echo", func(_ context.Context, args []Value, kwargs map[string]Value) (Value, error) {
			items := append([]Value{}, args...)
			for _, key := range []string{"extra"} {
				if v, ok := kwargs[key]; ok {
					items = append(items, v)
				}
			}
			return List(items...), nil
		})
		value, err := Eval(ctx, `echo(1, "a", extra=True)`, nil, WithFunctions(echo))
		if err != nil {
			t.Fatal(err)
		}
		if value.Len() != 3 || !value.Index(2).Bool() {
			t.Fatalf("value = %s", value)
		}
	})
}

func TestNewFunctionValidation(t *testing.T) {
	if _, err := NewFunction("bad", 42); err == nil {
		t.Fatal("non-func handler should error")
	}
	if _, err := NewFunction("bad", func() (int, int) { return 0, 0 }); err == nil {
		t.Fatal("second non-error return should error")
	}
	if _, err := NewFunction("bad", func(_ ...int) int { return 0 }); err == nil {
		t.Fatal("variadic handler should error")
	}
	if _, err := NewFunction("bad", func(_, _ int) int { return 0 }, WithParams("only")); err == nil {
		t.Fatal("WithParams count mismatch should error")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("MustFunction should panic on invalid handlers")
		}
	}()
	MustFunction("bad", "not a function")
}

func TestFunctionArgumentErrors(t *testing.T) {
	ctx := context.Background()
	add := MustFunction("add", func(a, b int) int { return a + b }, WithParams("a", "b"))

	cases := []string{
		"add(1)",         // missing argument
		"add(1, 2, 3)",   // too many
		"add(1, c=2)",    // unknown kwarg
		"add(1, 2, a=3)", // duplicate
		`add("x", 2)`,    // wrong type
	}
	for _, code := range cases {
		_, err := Eval(ctx, code, nil, WithFunctions(add))
		var execErr *ExecError
		if !errors.As(err, &execErr) {
			t.Errorf("%s: err = %v, want ExecError", code, err)
		}
	}
}

func TestPythonStub(t *testing.T) {
	type Forecast struct {
		City string
		High float64
	}
	forecast := MustFunction("get_forecast", func(_ context.Context, _, _ float64) (Forecast, error) {
		return Forecast{}, nil
	}, WithParams("lat", "lon"), WithDoc("Get the forecast."))

	stub := forecast.PythonStub()
	for _, want := range []string{
		"class Forecast(TypedDict):",
		"city: str",
		"high: float",
		"# Get the forecast.",
		"def get_forecast(lat: float, lon: float) -> Forecast: ...",
	} {
		if !strings.Contains(stub, want) {
			t.Errorf("stub missing %q:\n%s", want, stub)
		}
	}

	asyncFn := MustFunction("fetch", func(_ string) string { return "" }, WithAsync())
	if !strings.Contains(asyncFn.PythonStub(), "async def fetch(") {
		t.Errorf("async stub:\n%s", asyncFn.PythonStub())
	}

	custom := MustFunction("x", func() {}, WithStub("def x() -> None: ..."))
	if custom.PythonStub() != "def x() -> None: ..." {
		t.Errorf("WithStub override ignored: %q", custom.PythonStub())
	}

	raw := NewRawFunction("anything", func(context.Context, []Value, map[string]Value) (Value, error) {
		return None(), nil
	})
	if !strings.Contains(raw.PythonStub(), "*args: Any, **kwargs: Any") {
		t.Errorf("raw stub:\n%s", raw.PythonStub())
	}
}

func TestTypeCheckWithFunctionStubs(t *testing.T) {
	double := MustFunction("double", func(n int) int { return 2 * n }, WithParams("n"))
	// Passing a str where the stub declares int must fail the type check.
	_, err := Compile(`double("nope")`, WithFunctions(double), WithTypeCheck())
	var typeErr *TypeCheckError
	if !errors.As(err, &typeErr) {
		t.Fatalf("err = %v, want TypeCheckError", err)
	}
	// And a valid call passes.
	if _, err := Compile(`double(2)`, WithFunctions(double), WithTypeCheck()); err != nil {
		t.Fatalf("valid call failed type check: %v", err)
	}
}
