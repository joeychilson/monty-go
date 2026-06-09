package monty

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type tempArgs struct {
	Lat float64 `monty:"lat"`
	Lng float64 `monty:"lng"`
}

type forecast struct {
	City string  `monty:"city"`
	High float64 `monty:"high"`
	Low  float64 `monty:"low"`
}

func TestFunctionDispatch(t *testing.T) {
	getTemp := NewFunction("get_temperature", func(_ context.Context, _ tempArgs) (forecast, error) {
		return forecast{City: "San Francisco", High: 24.5, Low: 18.0}, nil
	}, WithDoc("Get the weather forecast for a coordinate."))

	if stub := getTemp.PythonStub(); !strings.Contains(stub, "def get_temperature") {
		t.Fatalf("stub missing function: %s", stub)
	}

	code := `
forecast = get_temperature(lat=lat, lng=lng)
f"{forecast['city']}: high {forecast['high']}C, low {forecast['low']}C"
`
	program, err := Compile(code, WithInputs("lat", "lng"), WithFunction(getTemp))
	if err != nil {
		t.Fatal(err)
	}
	defer program.Close()

	result, err := RunAs[string](context.Background(), program, Inputs{
		"lat": Float(37.7749),
		"lng": Float(-122.4194),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "San Francisco: high 24.5C, low 18.0C"
	if result != want {
		t.Fatalf("result = %q, want %q", result, want)
	}
}

func TestNewFunctionValidatesHandler(t *testing.T) {
	cases := []struct {
		name      string
		handler   any
		wantPanic bool
	}{
		// Invalid signatures must be rejected at registration, not at call time.
		{"non-func handler", "not a function", true},
		{"nil handler", nil, true},
		{"non-error second return", func() (int, string) { return 0, "" }, true},
		{"three returns", func() (int, int, error) { return 0, 0, nil }, true},
		{"three returns no error", func() (int, int, int) { return 0, 0, 0 }, true},

		// Valid signatures must be accepted.
		{"no returns", func(int) {}, false},
		{"single value return", func(int) int { return 0 }, false},
		{"single error return", func(int) error { return nil }, false},
		{"value and error", func(int) (int, error) { return 0, nil }, false},
		{"context value and error", func(context.Context, int) (int, error) { return 0, nil }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if tc.wantPanic && r == nil {
					t.Fatalf("NewFunction(%s) did not panic", tc.name)
				}
				if !tc.wantPanic && r != nil {
					t.Fatalf("NewFunction(%s) panicked unexpectedly: %v", tc.name, r)
				}
				if r != nil {
					if msg, ok := r.(string); !ok || !strings.Contains(msg, "monty: NewFunction") {
						t.Fatalf("panic message = %v, want one mentioning monty: NewFunction", r)
					}
				}
			}()
			_ = NewFunction("f", tc.handler)
		})
	}
}

// TestFunctionNonErrorSecondReturnNoIsNilPanic is the direct regression for the
// reported bug: a handler with a non-error second return used to reach
// callResults[1].IsNil() and panic with "reflect: call of reflect.Value.IsNil
// on string Value" on the first Python call. Registration-time validation now
// prevents the *Function from ever being constructed.
func TestFunctionNonErrorSecondReturnNoIsNilPanic(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected NewFunction to panic for func() (int, string)")
		}
	}()
	NewFunction("f", func() (int, string) { return 0, "" })
}

// TestFunctionValueAndErrorDispatch confirms a validated (value, error) handler
// still dispatches correctly through the slow call path: a nil error returns
// the value, a non-nil error is raised. A bool input field keeps it off the
// signed-int fast path, exercising call's callResults[1] handling directly.
func TestFunctionValueAndErrorDispatch(t *testing.T) {
	fn := NewFunction("maybe_fail", func(in struct {
		Fail bool `monty:"fail"`
	}) (string, error) {
		if in.Fail {
			return "", errors.New("requested failure")
		}
		return "ok", nil
	})

	program, err := Compile("maybe_fail(fail=fail)", WithInputs("fail"), WithFunction(fn))
	if err != nil {
		t.Fatal(err)
	}
	defer program.Close()

	got, err := RunAs[string](context.Background(), program, Inputs{"fail": Bool(false)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ok" {
		t.Fatalf("got %q, want %q", got, "ok")
	}

	if _, err := RunAs[string](context.Background(), program, Inputs{"fail": Bool(true)}); err == nil {
		t.Fatal("expected error from handler to surface, got nil")
	} else if !strings.Contains(err.Error(), "requested failure") {
		t.Fatalf("error = %v, want it to mention requested failure", err)
	}
}
