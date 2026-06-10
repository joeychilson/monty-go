package monty

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeychilson/monty/internal/ffi"
)

func TestRunMisuseMatrix(t *testing.T) {
	ctx := context.Background()
	prog, err := Compile(`step("one")`)
	if err != nil {
		t.Fatal(err)
	}
	defer prog.Close()

	run, err := prog.Start(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()

	if _, err := run.Result(); !errors.Is(err, ErrPaused) {
		t.Fatalf("Result while paused = %v, want ErrPaused", err)
	}
	call, ok := run.Pending().(*Call)
	if !ok {
		t.Fatalf("pending is %T", run.Pending())
	}
	if run.Pending() != Interrupt(call) {
		t.Fatal("Pending should return the same interrupt until resolved")
	}
	if err := call.NotHandled(ctx); err == nil {
		t.Fatal("NotHandled on a function call should error")
	}
	if err := call.Return(ctx, "done"); err != nil {
		t.Fatal(err)
	}
	if err := call.Return(ctx, "again"); !errors.Is(err, ErrResolved) {
		t.Fatalf("double resolve = %v, want ErrResolved", err)
	}
	if run.Paused() {
		t.Fatal("run should be finished")
	}
	if _, err := run.Dump(); !errors.Is(err, ErrNotPaused) {
		t.Fatalf("Dump after finish = %v, want ErrNotPaused", err)
	}
	if got, err := ResultAs[string](run); err != nil || got != "done" {
		t.Fatalf("result = %q, %v", got, err)
	}
	run.Close()
	// Result stays available after closing a finished run.
	if got, err := ResultAs[string](run); err != nil || got != "done" {
		t.Fatalf("result after close = %q, %v", got, err)
	}
}

func TestRunCloseWhilePaused(t *testing.T) {
	ctx := context.Background()
	prog, err := Compile(`pending()`)
	if err != nil {
		t.Fatal(err)
	}
	defer prog.Close()
	run, err := prog.Start(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	call, ok := run.Pending().(*Call)
	if !ok {
		t.Fatalf("pending is %T", run.Pending())
	}
	run.Close()
	if _, err := run.Result(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Result after close = %v, want ErrClosed", err)
	}
	if err := call.Return(ctx, 1); !errors.Is(err, ErrClosed) && !errors.Is(err, ErrResolved) {
		t.Fatalf("resume after close = %v", err)
	}
}

func TestRunRaiseTypedException(t *testing.T) {
	ctx := context.Background()
	code := `
try:
    risky()
except ValueError as exc:
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
	if err := call.Raise(ctx, Errorf("ValueError", "nope %d", 7)); err != nil {
		t.Fatal(err)
	}
	got, err := ResultAs[string](run)
	if err != nil {
		t.Fatal(err)
	}
	if got != "caught nope 7" {
		t.Fatalf("got %q", got)
	}
}

func TestRunManualGather(t *testing.T) {
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
			if len(req.CallIDs()) != 2 {
				t.Fatalf("pending ids = %v", req.CallIDs())
			}
			err := req.Resolve(ctx,
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

func TestStartSurfacesOSCallWithoutDispatch(t *testing.T) {
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
	if call.Name != string(OSPathReadText) {
		t.Fatalf("call name %q", call.Name)
	}
	if err := call.Return(ctx, "file-content"); err != nil {
		t.Fatal(err)
	}
	got, err := ResultAs[string](run)
	if err != nil || got != "file-content" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestStartNotHandledOSCall(t *testing.T) {
	ctx := context.Background()
	prog, err := Compile(`
from pathlib import Path
try:
    Path("/data/x.txt").read_text()
    result = "read"
except PermissionError:
    result = "denied"
result
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
	if err := call.NotHandled(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := ResultAs[string](run)
	if err != nil || got != "denied" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestProgramDumpLoad(t *testing.T) {
	prog, err := Compile("n * 2", WithInputs("n"), WithScriptName("double.py"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := prog.Dump()
	if err != nil {
		t.Fatal(err)
	}
	prog.Close()

	loaded, err := LoadProgram(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Close()
	if loaded.ScriptName() != "double.py" {
		t.Fatalf("script name %q", loaded.ScriptName())
	}
	if names := loaded.InputNames(); len(names) != 1 || names[0] != "n" {
		t.Fatalf("input names %v", names)
	}
	if loaded.Code() != "n * 2" {
		t.Fatalf("code %q", loaded.Code())
	}
	got, err := RunAs[int](context.Background(), loaded, map[string]any{"n": 21})
	if err != nil || got != 42 {
		t.Fatalf("got %d, %v", got, err)
	}
}

func TestProgramClosedOperations(t *testing.T) {
	prog, err := Compile("1")
	if err != nil {
		t.Fatal(err)
	}
	prog.Close()
	if _, err := prog.Run(context.Background(), nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Run on closed = %v", err)
	}
	if _, err := prog.Dump(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Dump on closed = %v", err)
	}
}

func TestConcurrentRuns(t *testing.T) {
	prog, err := Compile("n * n", WithInputs("n"))
	if err != nil {
		t.Fatal(err)
	}
	defer prog.Close()
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 50 {
				n := i*100 + j
				got, err := RunAs[int](context.Background(), prog, map[string]any{"n": n})
				if err != nil || got != n*n {
					failures.Add(1)
					return
				}
			}
		}()
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d goroutines failed", failures.Load())
	}
}

func TestRunJSON(t *testing.T) {
	prog, err := Compile(`{"pair": (1, 2), "n": 7}`)
	if err != nil {
		t.Fatal(err)
	}
	defer prog.Close()
	data, err := prog.RunJSON(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"$tuple"`) {
		t.Fatalf("json = %s", data)
	}
}

func TestDeadlineWrapsExecError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := Eval(ctx, `
n = 0
while True:
    n = n + 1
n
`, nil)
	if err == nil {
		t.Fatal("expected a deadline failure")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v does not match context.DeadlineExceeded", err)
	}
	var execErr *ExecError
	if !errors.As(err, &execErr) {
		t.Fatalf("err %v does not expose *ExecError", err)
	}
	if !errors.Is(err, ErrTimeLimit) && execErr.Type != "KeyboardInterrupt" {
		t.Fatalf("unexpected exec error type %q", execErr.Type)
	}
}

func TestMemoryAndRecursionLimits(t *testing.T) {
	_, err := Eval(context.Background(), `[0] * 10_000_000`, nil,
		WithLimits(Limits{MaxMemory: 1 << 20}))
	if !errors.Is(err, ErrMemoryLimit) {
		t.Fatalf("memory err = %v", err)
	}

	_, err = Eval(context.Background(), `
def f(n):
    return f(n + 1)
f(0)
`, nil, WithLimits(Limits{MaxRecursionDepth: 30}))
	if !errors.Is(err, ErrRecursionLimit) {
		t.Fatalf("recursion err = %v", err)
	}
}

func TestStderrTaggedPrintDecode(t *testing.T) {
	// Nothing emits stderr upstream yet, so exercise the tagged decoder
	// against a synthetic payload.
	var payload bytes.Buffer
	writeChunk := func(stream byte, text string) {
		payload.WriteByte(stream)
		var size [4]byte
		size[0] = byte(len(text)) //nolint:gosec // test chunks are far below 256 bytes
		payload.Write(size[:])
		payload.WriteString(text)
	}
	writeChunk(0, "to stdout\n")
	writeChunk(1, "to stderr\n")
	writeChunk(0, "more stdout\n")

	var stdout, stderr bytes.Buffer
	config := runConfig{stdout: &stdout, stderr: &stderr}
	printed := ffi.Printed{Flags: ffi.PrintTagged, Data: payload.String()}
	if err := writePrint(&config, printed); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "to stdout\nmore stdout\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if stderr.String() != "to stderr\n" {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestStdoutAcrossResumeHops(t *testing.T) {
	ctx := context.Background()
	prog, err := Compile(`
print("before")
x = ask()
print("after", x)
x
`)
	if err != nil {
		t.Fatal(err)
	}
	defer prog.Close()
	var out bytes.Buffer
	run, err := prog.Start(ctx, nil, WithStdout(&out))
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	call, ok := run.Pending().(*Call)
	if !ok {
		t.Fatalf("pending is %T", run.Pending())
	}
	if out.String() != "before\n" {
		t.Fatalf("stdout before resume = %q", out.String())
	}
	if err := call.Return(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if out.String() != "before\nafter 42\n" {
		t.Fatalf("stdout after resume = %q", out.String())
	}
}

// hammerGC spins a sibling goroutine that repeatedly forces a garbage
// collection until the returned stop function is called.
func hammerGC(t *testing.T) (stop func()) {
	t.Helper()
	var done atomic.Bool
	var wg sync.WaitGroup
	wg.Go(func() {
		for !done.Load() {
			runtime.GC()
		}
	})
	return func() {
		done.Store(true)
		wg.Wait()
	}
}

// TestEvalStringsSurviveGC guards the keep-alive discipline: every string
// threaded into the single-hop FFI call must stay rooted for the whole call.
func TestEvalStringsSurviveGC(t *testing.T) {
	ctx := context.Background()
	stop := hammerGC(t)
	defer stop()

	const iterations = 2000
	for i := range iterations {
		code := fmt.Sprintf("# iteration %d %s\nprefix + body", i, strings.Repeat("x", 48))
		prefix := fmt.Sprintf("prefix-%d-", i)
		body := strings.Repeat(fmt.Sprintf("b%d", i), 8)
		want := prefix + body

		got, err := EvalAs[string](ctx, code, map[string]Value{
			"prefix": Str(prefix),
			"body":   Str(body),
		})
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if got != want {
			t.Fatalf("iteration %d: got %q, want %q", i, got, want)
		}
	}
}

func TestHostFunctionPanicBecomesPythonError(t *testing.T) {
	boom := MustFunction("boom", func(_ int) int { panic("kaboom") })
	_, err := Eval(context.Background(), "boom(1)", nil, WithFunctions(boom))
	var execErr *ExecError
	if !errors.As(err, &execErr) {
		t.Fatalf("err = %T (%v)", err, err)
	}
	if execErr.Type != "RuntimeError" || !strings.Contains(execErr.Message, "kaboom") {
		t.Fatalf("exec err = %+v", execErr)
	}

	// The same guarantee on the Start-machine dispatch path.
	slow := MustFunction("boom", func(_ string) string { panic("slow kaboom") })
	prog, err := Compile(`boom("x")`, WithFunctions(slow))
	if err != nil {
		t.Fatal(err)
	}
	defer prog.Close()
	run, err := prog.Start(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	_, err = run.Result()
	if !errors.As(err, &execErr) || !strings.Contains(execErr.Message, "slow kaboom") {
		t.Fatalf("start-path err = %v", err)
	}
}

func TestTracebackFrames(t *testing.T) {
	_, err := Eval(context.Background(), `
def inner():
    raise KeyError("missing")

def outer():
    return inner()

outer()
`, nil)
	var execErr *ExecError
	if !errors.As(err, &execErr) {
		t.Fatalf("err = %T", err)
	}
	if len(execErr.Traceback) < 3 {
		t.Fatalf("frames = %d, want >= 3", len(execErr.Traceback))
	}
	functions := make([]string, 0, len(execErr.Traceback))
	for _, frame := range execErr.Traceback {
		if frame.File != "main.py" {
			t.Errorf("frame file = %q", frame.File)
		}
		if frame.Line <= 0 {
			t.Errorf("frame line = %d", frame.Line)
		}
		functions = append(functions, frame.Function)
	}
	joined := strings.Join(functions, ",")
	if !strings.Contains(joined, "inner") || !strings.Contains(joined, "outer") {
		t.Fatalf("functions = %v", functions)
	}
}
