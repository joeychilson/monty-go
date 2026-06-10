package monty

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestREPLStatePersists(t *testing.T) {
	ctx := context.Background()
	repl, err := NewREPL()
	if err != nil {
		t.Fatal(err)
	}
	defer repl.Close()

	if _, err := repl.Eval(ctx, "total = 0", nil); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := repl.Eval(ctx, "total = total + n", map[string]any{"n": i}); err != nil {
			t.Fatal(err)
		}
	}
	value, err := repl.Eval(ctx, "total", nil)
	if err != nil || value.Int() != 6 {
		t.Fatalf("total = %v, %v", value, err)
	}
}

func TestREPLDumpLoad(t *testing.T) {
	ctx := context.Background()
	repl, err := NewREPL()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repl.Eval(ctx, "def greet(name):\n    return 'hi ' + name", nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err := repl.Dump()
	if err != nil {
		t.Fatal(err)
	}
	repl.Close()

	restored, err := LoadREPL(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if !restored.HasFunction("greet") {
		t.Fatal("restored session lost greet")
	}
	names, err := restored.FunctionNames()
	if err != nil || len(names) != 1 || names[0] != "greet" {
		t.Fatalf("names = %v, %v", names, err)
	}
	value, err := restored.Call(ctx, "greet", "monty")
	if err != nil || value.Str() != "hi monty" {
		t.Fatalf("call = %v, %v", value, err)
	}
}

func TestREPLStartAndBusy(t *testing.T) {
	ctx := context.Background()
	repl, err := NewREPL()
	if err != nil {
		t.Fatal(err)
	}
	defer repl.Close()

	if _, err := repl.Eval(ctx, "base = 40", nil); err != nil {
		t.Fatal(err)
	}
	run, err := repl.Start(ctx, "base + offset()", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()

	// The session is mid-snippet: other REPL methods refuse.
	if _, err := repl.Eval(ctx, "1", nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("Eval while busy = %v, want ErrBusy", err)
	}
	if _, err := repl.Dump(); !errors.Is(err, ErrBusy) {
		t.Fatalf("Dump while busy = %v, want ErrBusy", err)
	}

	call, ok := run.Pending().(*Call)
	if !ok {
		t.Fatalf("pending is %T", run.Pending())
	}
	if err := call.Return(ctx, 2); err != nil {
		t.Fatal(err)
	}
	got, err := ResultAs[int](run)
	if err != nil || got != 42 {
		t.Fatalf("result = %d, %v", got, err)
	}

	// Session handed back: usable again with state intact.
	value, err := repl.Eval(ctx, "base", nil)
	if err != nil || value.Int() != 40 {
		t.Fatalf("after start: %v, %v", value, err)
	}
}

func TestREPLMidSnippetDumpLoad(t *testing.T) {
	ctx := context.Background()
	repl, err := NewREPL()
	if err != nil {
		t.Fatal(err)
	}
	defer repl.Close()
	if _, err := repl.Eval(ctx, "prefix = 'answer: '", nil); err != nil {
		t.Fatal(err)
	}
	run, err := repl.Start(ctx, "prefix + str(compute())", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := run.Pending().(*Call); !ok {
		t.Fatalf("pending is %T", run.Pending())
	}
	snapshot, err := run.Dump()
	if err != nil {
		t.Fatal(err)
	}
	run.Close()

	// Restore in a "new process": both the paused run and its session.
	restoredREPL, restoredRun, err := LoadREPLRun(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer restoredREPL.Close()
	defer restoredRun.Close()
	if _, err := restoredREPL.Eval(ctx, "1", nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("restored session should be busy, got %v", err)
	}
	call, ok := restoredRun.Pending().(*Call)
	if !ok || call.Name != "compute" {
		t.Fatalf("pending = %#v", restoredRun.Pending())
	}
	if err := call.Return(ctx, 42); err != nil {
		t.Fatal(err)
	}
	got, err := ResultAs[string](restoredRun)
	if err != nil || got != "answer: 42" {
		t.Fatalf("result %q, %v", got, err)
	}
	// Session usable, with the pre-snippet state.
	value, err := restoredREPL.Eval(ctx, "prefix", nil)
	if err != nil || value.Str() != "answer: " {
		t.Fatalf("restored prefix = %v, %v", value, err)
	}
}

func TestREPLTypeCheckAccumulation(t *testing.T) {
	ctx := context.Background()
	repl, err := NewREPL(WithTypeCheck())
	if err != nil {
		t.Fatal(err)
	}
	defer repl.Close()

	if _, err := repl.Eval(ctx, "count: int = 1", nil); err != nil {
		t.Fatal(err)
	}
	// Later snippets see earlier definitions in the accumulated context.
	if _, err := repl.Eval(ctx, "count + 1", nil); err != nil {
		t.Fatal(err)
	}
	// A type error is caught before execution.
	_, err = repl.Eval(ctx, `count + "nope"`, nil)
	var typeErr *TypeCheckError
	if !errors.As(err, &typeErr) {
		t.Fatalf("err = %v, want TypeCheckError", err)
	}
	// WithoutTypeCheck skips the check and stays out of the context.
	if _, err := repl.Eval(ctx, "untyped = some_name_the_checker_would_reject if False else 1", nil, WithoutTypeCheck()); err != nil {
		t.Fatalf("skip-check eval failed: %v", err)
	}
	if err := repl.TypeCheck("count + 1"); err != nil {
		t.Fatalf("TypeCheck = %v", err)
	}
}

func TestREPLPerSnippetLimits(t *testing.T) {
	ctx := context.Background()
	repl, err := NewREPL()
	if err != nil {
		t.Fatal(err)
	}
	defer repl.Close()
	_, err = repl.Eval(ctx, `
n = 0
while True:
    n = n + 1
`, nil, WithLimits(Limits{MaxDuration: 30 * time.Millisecond}))
	if !errors.Is(err, ErrTimeLimit) {
		t.Fatalf("err = %v, want ErrTimeLimit", err)
	}
	// The session survives the limit failure.
	value, err := repl.Eval(ctx, "40 + 2", nil)
	if err != nil || value.Int() != 42 {
		t.Fatalf("after limit: %v, %v", value, err)
	}
}

func TestREPLCancellation(t *testing.T) {
	repl, err := NewREPL()
	if err != nil {
		t.Fatal(err)
	}
	defer repl.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err = repl.Eval(ctx, `
n = 0
while True:
    n = n + 1
`, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// The session survives cancellation.
	value, err := repl.Eval(context.Background(), "'still alive'", nil)
	if err != nil || value.Str() != "still alive" {
		t.Fatalf("after cancel: %v, %v", value, err)
	}
}

func TestREPLStdout(t *testing.T) {
	ctx := context.Background()
	var out bytes.Buffer
	repl, err := NewREPL(WithStdout(&out))
	if err != nil {
		t.Fatal(err)
	}
	defer repl.Close()
	if _, err := repl.Eval(ctx, `print("session output")`, nil); err != nil {
		t.Fatal(err)
	}
	if out.String() != "session output\n" {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestREPLErrorKeepsSession(t *testing.T) {
	ctx := context.Background()
	repl, err := NewREPL()
	if err != nil {
		t.Fatal(err)
	}
	defer repl.Close()
	if _, err := repl.Eval(ctx, "kept = 'yes'", nil); err != nil {
		t.Fatal(err)
	}
	_, err = repl.Eval(ctx, "1 / 0", nil)
	var execErr *ExecError
	if !errors.As(err, &execErr) || execErr.Type != "ZeroDivisionError" {
		t.Fatalf("err = %v", err)
	}
	value, err := repl.Eval(ctx, "kept", nil)
	if err != nil || value.Str() != "yes" {
		t.Fatalf("session lost after error: %v, %v", value, err)
	}

	// The same guarantee through Start: a failing snippet hands the session back.
	run, err := repl.Start(ctx, "undefined_name + 1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if lookup, ok := run.Pending().(*NameLookup); ok {
		if err := lookup.Undefined(ctx); err == nil {
			_, _ = run.Result() //nolint:errcheck // probing terminal state
		}
	}
	_, resultErr := run.Result()
	if resultErr == nil {
		t.Fatal("expected the snippet to fail")
	}
	run.Close()
	value, err = repl.Eval(ctx, "kept", nil)
	if err != nil || value.Str() != "yes" {
		t.Fatalf("session lost after failed start: %v, %v", value, err)
	}
}

func TestDetectContinuation(t *testing.T) {
	cases := []struct {
		code string
		want ContinuationMode
	}{
		{"x = 1", ContinuationComplete},
		{"x = [1,", ContinuationImplicit},
		{"def f():", ContinuationBlock},
	}
	for _, tc := range cases {
		got, err := DetectContinuation(tc.code)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("DetectContinuation(%q) = %s, want %s", tc.code, got, tc.want)
		}
	}
}
