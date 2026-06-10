package monty

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Defer maps to Monty's resume_pending, which is only valid for function
// calls; the binding must reject it on OS calls without consuming the run.
func TestDeferOnOSCallRejected(t *testing.T) {
	ctx := context.Background()
	prog, err := Compile(`
from pathlib import Path
Path("/data/x.txt").read_text()
`)
	if err != nil {
		t.Fatal(err)
	}
	defer prog.Close()
	run, err := prog.Start(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	call, ok := run.Pending().(*Call)
	if !ok || !call.OS {
		t.Fatalf("pending = %#v", run.Pending())
	}
	if err := call.Defer(ctx); err == nil || !strings.Contains(err.Error(), "Defer is only valid") {
		t.Fatalf("Defer on OS call = %v, want descriptive error", err)
	}
	if !run.Paused() {
		t.Fatal("run must stay paused after a rejected Defer")
	}
	if err := call.Return(ctx, "content"); err != nil {
		t.Fatal(err)
	}
	if got, err := ResultAs[string](run); err != nil || got != "content" {
		t.Fatalf("got %q, %v", got, err)
	}
}

// An exception type Monty does not know must degrade to a catchable
// RuntimeError instead of aborting the run.
func TestRaiseUnknownExceptionTypeFallsBack(t *testing.T) {
	ctx := context.Background()
	code := `
try:
    risky()
except RuntimeError as exc:
    result = f"caught {exc}"
result
`
	prog, err := Compile(code)
	if err != nil {
		t.Fatal(err)
	}
	defer prog.Close()
	run, err := prog.Start(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	call, ok := run.Pending().(*Call)
	if !ok {
		t.Fatalf("pending is %T", run.Pending())
	}
	if err := call.Raise(ctx, Errorf("MyCustomError", "boom")); err != nil {
		t.Fatal(err)
	}
	got, err := ResultAs[string](run)
	if err != nil {
		t.Fatal(err)
	}
	if got != "caught MyCustomError: boom" {
		t.Fatalf("got %q", got)
	}
}

// Same fallback on the single-hop host-callback path: the wrapped run keeps
// going and the Python try/except sees a RuntimeError.
func TestHostFunctionUnknownExceptionCatchable(t *testing.T) {
	ctx := context.Background()
	fail := MustFunction("fail", func() (int, error) {
		return 0, Errorf("WeirdGoError", "nope")
	})
	got, err := EvalAs[string](ctx, `
try:
    fail()
    result = "no error"
except RuntimeError as exc:
    result = f"caught {exc}"
result
`, nil, WithFunctions(fail))
	if err != nil {
		t.Fatal(err)
	}
	if got != "caught WeirdGoError: nope" {
		t.Fatalf("got %q", got)
	}
}

type panicWriter struct{}

func (panicWriter) Write([]byte) (int, error) { panic("writer exploded") }

// A panicking user Writer must surface as an error, not unwind through the
// Rust print callback frame.
func TestPrintWriterPanicContained(t *testing.T) {
	ctx := context.Background()
	_, err := Eval(ctx, `print("hello")`, nil, WithStdout(panicWriter{}))
	if err == nil || !strings.Contains(err.Error(), "print writer panicked") {
		t.Fatalf("err = %v, want print writer panic error", err)
	}
}

// Closing a paused REPL run loses the session that moved into it; the REPL
// must report that clearly instead of returning ErrBusy forever.
func TestREPLSessionLostAfterRunClose(t *testing.T) {
	ctx := context.Background()
	repl, err := NewREPL()
	if err != nil {
		t.Fatal(err)
	}
	defer repl.Close()
	if _, err := repl.Eval(ctx, "x = 1", nil); err != nil {
		t.Fatal(err)
	}
	run, err := repl.Start(ctx, "pending()", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !run.Paused() {
		t.Fatalf("run should pause; result state: %v", run.Pending())
	}
	if _, err := repl.Eval(ctx, "x", nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("Eval while busy = %v, want ErrBusy", err)
	}
	run.Close()
	_, err = repl.Eval(ctx, "x", nil)
	if errors.Is(err, ErrBusy) {
		t.Fatalf("Eval after closing the paused run still reports ErrBusy: %v", err)
	}
	if !errors.Is(err, ErrClosed) || !strings.Contains(err.Error(), "session lost") {
		t.Fatalf("Eval after losing the session = %v, want session-lost error wrapping ErrClosed", err)
	}
}

// A bad caller-supplied future value must fail Gather.Resolve before any
// async-owned results are consumed, leaving the gather fully retryable.
func TestGatherBadFutureValueRetryable(t *testing.T) {
	ctx := context.Background()
	code := `
import asyncio

async def main():
    a, b = await asyncio.gather(foo(), bar())
    return a + b

await main()
`
	prog, err := Compile(code)
	if err != nil {
		t.Fatal(err)
	}
	defer prog.Close()
	run, err := prog.Start(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()

	ids := map[string]CallID{}
	for run.Paused() {
		switch req := run.Pending().(type) {
		case *NameLookup:
			if err := req.Return(ctx, ExternalFunction(req.Name)); err != nil {
				t.Fatal(err)
			}
		case *Call:
			ids[req.Name] = req.ID
			if err := req.Defer(ctx); err != nil {
				t.Fatal(err)
			}
		case *Gather:
			// A zero Value passes From but fails raw encoding; the error
			// must leave the gather pending and retryable.
			err := req.Resolve(ctx,
				FutureValue(ids["foo"], Value{}),
				FutureValue(ids["bar"], 32),
			)
			if err == nil {
				t.Fatal("Resolve with an invalid value should fail")
			}
			if !run.Paused() || run.Pending() != Interrupt(req) {
				t.Fatal("gather must stay pending after a rejected Resolve")
			}
			err = req.Resolve(ctx,
				FutureValue(ids["foo"], 10),
				FutureValue(ids["bar"], 32),
			)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := ResultAs[int](run)
	if err != nil {
		t.Fatal(err)
	}
	if got != 42 {
		t.Fatalf("got %d", got)
	}
}

// Running a closed Program with a pooled output that previously carried an
// owned raw handle must fail cleanly (no double free of the stale handle).
func TestRunAfterCloseWithOwnedHandleResult(t *testing.T) {
	ctx := context.Background()
	prog, err := Compile(`
from datetime import date
date(2026, 6, 9)
`)
	if err != nil {
		t.Fatal(err)
	}
	// Dates decode through an owned raw handle, exercising the pooled
	// fast-output consume path.
	value, err := prog.Run(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if value.Date() != (Date{Year: 2026, Month: 6, Day: 9}) {
		t.Fatalf("date = %#v", value.Date())
	}
	prog.Close()
	for range 8 {
		if _, err := prog.Run(ctx, nil); !errors.Is(err, ErrClosed) {
			t.Fatalf("run after close = %v, want ErrClosed", err)
		}
	}
}
