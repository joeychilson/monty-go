package monty

import (
	"context"
	"strings"
	"testing"
)

type boomArgs struct {
	X int `monty:"x"`
}

// TestHostFunctionPanicBecomesException guards the fix for CODE_REVIEW.md §1.3:
// a panic in a host-function handler must not unwind across the Rust FFI
// boundary. The handler below qualifies for the fast callback path
// (struct-of-ints input, signed-int output), so Rust invokes it synchronously
// in the middle of mg_program_run_host_raw. Before the fix the panic unwound
// through the Rust frames and aborted the process; after it, the panic is
// converted into a Python RuntimeError surfaced as a Go error.
func TestHostFunctionPanicBecomesException(t *testing.T) {
	boom := NewFunction("boom", func(boomArgs) int {
		panic("kaboom")
	})

	program, err := Compile("boom(1)", WithFunction(boom))
	if err != nil {
		t.Fatal(err)
	}
	defer program.Close()

	// canUseFunctionCallbackFast is what routes Run through the synchronous
	// callback path the fix protects. If this ever stops holding, the test is
	// no longer exercising §1.3.
	if !program.runConfig().canUseFunctionCallbackFast() {
		t.Fatal("expected the fast callback path to be used for this handler")
	}

	_, err = program.Run(context.Background(), nil)
	if err == nil {
		t.Fatal("expected an error from a panicking host function, got nil")
	}
	if !strings.Contains(err.Error(), "host function panicked") {
		t.Fatalf("error = %q, want it to mention the host function panic", err.Error())
	}
	if !strings.Contains(err.Error(), "kaboom") {
		t.Fatalf("error = %q, want it to include the panic value", err.Error())
	}
}

// TestHostFunctionPanicIsCatchableInPython confirms the converted panic is a
// genuine, catchable Python RuntimeError rather than an opaque abort.
func TestHostFunctionPanicIsCatchableInPython(t *testing.T) {
	boom := NewFunction("boom", func(boomArgs) int {
		panic("kaboom")
	})

	code := `
try:
    boom(1)
    result = "no-raise"
except RuntimeError as e:
    result = "caught: " + str(e)
result
`
	program, err := Compile(code, WithFunction(boom))
	if err != nil {
		t.Fatal(err)
	}
	defer program.Close()

	got, err := RunAs[string](context.Background(), program, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "caught: ") || !strings.Contains(got, "host function panicked") {
		t.Fatalf("result = %q, want a caught RuntimeError mentioning the host panic", got)
	}
}
