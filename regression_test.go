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

// REPL history is part of the session's module, not an external stub: an
// unannotated name rebound to an incompatible type across snippets is the
// ordinary REPL behavior and must not be rejected. Routing history through the
// stubs channel froze each binding's declared type and reported a spurious
// invalid-assignment (e.g. list is not assignable to Literal["hi"]).
func TestREPLUnannotatedRebindAcrossSnippets(t *testing.T) {
	ctx := context.Background()
	repl, err := NewREPL(WithTypeCheck())
	if err != nil {
		t.Fatal(err)
	}
	defer repl.Close()

	if _, err := repl.Eval(ctx, `x = "hi"`, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := repl.Eval(ctx, `x = [1, 2, 3]`, nil); err != nil {
		t.Fatalf("rebinding an unannotated name to a new type = %v, want clean", err)
	}
	// The most ordinary case of all: incrementing a counter on a later line.
	if _, err := repl.Eval(ctx, `n = 0`, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := repl.Eval(ctx, `n = n + 1`, nil); err != nil {
		t.Fatalf("counter increment across snippets = %v, want clean", err)
	}
}

// Moving history into the module must not weaken real type enforcement: an
// annotated binding keeps its declared type, so an incompatible reassignment in
// a later snippet still fails, and explicit WithStubs declarations stay
// authoritative.
func TestREPLDeclaredTypesStillEnforced(t *testing.T) {
	ctx := context.Background()
	repl, err := NewREPL(WithTypeCheck())
	if err != nil {
		t.Fatal(err)
	}
	defer repl.Close()

	if _, err := repl.Eval(ctx, `count: int = 1`, nil); err != nil {
		t.Fatal(err)
	}
	var typeErr *TypeCheckError
	if _, err := repl.Eval(ctx, `count = "nope"`, nil); !errors.As(err, &typeErr) {
		t.Fatalf("reassigning an int-declared name to str = %v, want TypeCheckError", err)
	}

	stubbed, err := NewREPL(WithStubs("config: dict[str, int]"))
	if err != nil {
		t.Fatal(err)
	}
	defer stubbed.Close()
	if err := stubbed.TypeCheck(`config = "nope"`); !errors.As(err, &typeErr) {
		t.Fatalf("reassigning a stub-declared name = %v, want TypeCheckError", err)
	}
}

// Diagnostics for a snippet checked behind accumulated history must report the
// lines the user actually typed, not their position in the prepended module.
func TestREPLDiagnosticsAreSnippetRelative(t *testing.T) {
	ctx := context.Background()
	repl, err := NewREPL(WithTypeCheck(), WithScriptName("session.py"))
	if err != nil {
		t.Fatal(err)
	}
	defer repl.Close()

	// Accumulate a few lines of history so the snippet is no longer at line 1.
	if _, err := repl.Eval(ctx, "a = 1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := repl.Eval(ctx, "b = 2", nil); err != nil {
		t.Fatal(err)
	}

	// The error sits on the snippet's own line 2 (c is an int).
	err = repl.TypeCheck("c = 3\nc + \"x\"")
	var typeErr *TypeCheckError
	if !errors.As(err, &typeErr) {
		t.Fatalf("err = %v, want TypeCheckError", err)
	}
	if len(typeErr.Diagnostics) == 0 {
		t.Fatal("no diagnostics")
	}
	if got := typeErr.Diagnostics[0].Line; got != 2 {
		t.Fatalf("diagnostic line = %d, want 2 (snippet-relative)", got)
	}
	if got := typeErr.Render(DiagnosticConcise, false); !strings.Contains(got, "session.py:2:") {
		t.Fatalf("concise render = %q, want a session.py:2: location", got)
	}
}

// The history offset must compose with the stub-import line that Monty injects
// internally for genuine stubs: a snippet checked behind both a stub and
// accumulated history still reports snippet-relative lines.
func TestREPLDiagnosticsComposeWithStubs(t *testing.T) {
	ctx := context.Background()
	repl, err := NewREPL(WithTypeCheck(), WithStubs("limit: int"), WithScriptName("session.py"))
	if err != nil {
		t.Fatal(err)
	}
	defer repl.Close()

	if _, err := repl.Eval(ctx, "a = 1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := repl.Eval(ctx, "d = 2", nil); err != nil {
		t.Fatal(err)
	}

	// Line 1 reads the genuine stub; line 2 is the error (int + str).
	err = repl.TypeCheck("c = limit\nc + \"x\"")
	var typeErr *TypeCheckError
	if !errors.As(err, &typeErr) || len(typeErr.Diagnostics) == 0 {
		t.Fatalf("err = %v, want TypeCheckError", err)
	}
	if got := typeErr.Diagnostics[0].Line; got != 2 {
		t.Fatalf("diagnostic line = %d, want 2 (snippet-relative)", got)
	}
}
