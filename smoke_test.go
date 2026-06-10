package monty

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSmokeDirectRun(t *testing.T) {
	prog, err := Compile("x * x + y * y", WithInputs("x", "y"))
	if err != nil {
		t.Fatal(err)
	}
	defer prog.Close()

	type coords struct{ X, Y int }
	got, err := RunAs[int](context.Background(), prog, coords{X: 3, Y: 4})
	if err != nil {
		t.Fatal(err)
	}
	if got != 25 {
		t.Fatalf("got %d, want 25", got)
	}
}

func TestSmokeEval(t *testing.T) {
	value, err := Eval(context.Background(), `", ".join(words)`, map[string]any{"words": []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if value.Str() != "a, b" {
		t.Fatalf("got %q", value.Str())
	}
}

func TestSmokeHostFunctionSingleHop(t *testing.T) {
	double := MustFunction("double", func(n int) int { return 2 * n })
	got, err := EvalAs[int](context.Background(), "double(21)", nil, WithFunctions(double))
	if err != nil {
		t.Fatal(err)
	}
	if got != 42 {
		t.Fatalf("got %d, want 42", got)
	}
}

func TestSmokeHostFunctionStrings(t *testing.T) {
	greet := MustFunction("greet", func(name string) string { return "hello " + name })
	got, err := EvalAs[string](context.Background(), `greet("monty")`, nil, WithFunctions(greet))
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello monty" {
		t.Fatalf("got %q", got)
	}
}

func TestSmokeStdout(t *testing.T) {
	var out bytes.Buffer
	_, err := Eval(context.Background(), "print('hi'); print('there'); 1", nil, WithStdout(&out))
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "hi\nthere\n" {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestSmokeExecError(t *testing.T) {
	_, err := Eval(context.Background(), "1 / 0", nil)
	var execErr *ExecError
	if !errors.As(err, &execErr) {
		t.Fatalf("error is %T (%v), want *ExecError", err, err)
	}
	if execErr.Type != "ZeroDivisionError" {
		t.Fatalf("Type = %q", execErr.Type)
	}
	if len(execErr.Traceback) == 0 {
		t.Fatal("expected traceback frames")
	}
	if !strings.Contains(execErr.Render(FormatTraceback), "ZeroDivisionError") {
		t.Fatalf("traceback render = %q", execErr.Render(FormatTraceback))
	}
}

func TestSmokeSyntaxError(t *testing.T) {
	_, err := Compile("def broken(:")
	var synErr *SyntaxError
	if !errors.As(err, &synErr) {
		t.Fatalf("error is %T (%v), want *SyntaxError", err, err)
	}
}

func TestSmokeStartResume(t *testing.T) {
	prog, err := Compile(`ask("q") + "!"`)
	if err != nil {
		t.Fatal(err)
	}
	defer prog.Close()

	run, err := prog.Start(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()

	// A direct call of an unknown name may surface as a NameLookup first or
	// as a FunctionCall straight away, depending on the interpreter.
	if lookup, ok := run.Pending().(*NameLookup); ok {
		if lookup.Name != "ask" {
			t.Fatalf("lookup name %q", lookup.Name)
		}
		if err := lookup.Return(context.Background(), ExternalFunction("ask")); err != nil {
			t.Fatal(err)
		}
	}
	call, ok := run.Pending().(*Call)
	if !ok {
		t.Fatalf("pending is %T", run.Pending())
	}
	if call.Name != "ask" || call.Args[0].Str() != "q" {
		t.Fatalf("call %q args %v", call.Name, call.Args)
	}
	if err := call.Return(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	got, err := ResultAs[string](run)
	if err != nil {
		t.Fatal(err)
	}
	if got != "a!" {
		t.Fatalf("got %q", got)
	}
}

func TestSmokeDumpLoadResume(t *testing.T) {
	prog, err := Compile(`fetch(7) * 2`)
	if err != nil {
		t.Fatal(err)
	}
	defer prog.Close()

	run, err := prog.Start(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if lookup, ok := run.Pending().(*NameLookup); ok {
		if err := lookup.Return(context.Background(), ExternalFunction("fetch")); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := run.Pending().(*Call); !ok {
		t.Fatalf("pending is %T", run.Pending())
	}
	snapshot, err := run.Dump()
	if err != nil {
		t.Fatal(err)
	}
	run.Close()

	restored, err := LoadRun(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	call, ok := restored.Pending().(*Call)
	if !ok {
		t.Fatalf("restored pending is %T", restored.Pending())
	}
	if call.Args[0].Int() != 7 {
		t.Fatalf("restored arg %v", call.Args[0])
	}
	if err := call.Return(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	got, err := ResultAs[int](restored)
	if err != nil {
		t.Fatal(err)
	}
	if got != 20 {
		t.Fatalf("got %d", got)
	}
}

func TestSmokeAsyncGather(t *testing.T) {
	fetch := MustFunction("fetch", func(n int) int {
		time.Sleep(10 * time.Millisecond)
		return n * 10
	}, WithAsync())
	code := `
import asyncio

async def main():
    a, b = await asyncio.gather(fetch(1), fetch(2))
    return a + b

await main()
`
	prog, err := Compile(code, WithFunctions(fetch))
	if err != nil {
		t.Fatal(err)
	}
	defer prog.Close()
	started := time.Now()
	got, err := RunAs[int](context.Background(), prog, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != 30 {
		t.Fatalf("got %d, want 30", got)
	}
	if elapsed := time.Since(started); elapsed > 60*time.Millisecond {
		t.Fatalf("gather did not run concurrently: %v", elapsed)
	}
}

func TestSmokeREPL(t *testing.T) {
	repl, err := NewREPL()
	if err != nil {
		t.Fatal(err)
	}
	defer repl.Close()

	if _, err := repl.Eval(context.Background(), "x = 40", nil); err != nil {
		t.Fatal(err)
	}
	value, err := repl.Eval(context.Background(), "x + 2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if value.Int() != 42 {
		t.Fatalf("got %d", value.Int())
	}
}

func TestSmokeREPLHostFunction(t *testing.T) {
	bump := MustFunction("bump", func(n int) int { return n + 1 })
	repl, err := NewREPL(WithFunctions(bump))
	if err != nil {
		t.Fatal(err)
	}
	defer repl.Close()
	value, err := repl.Eval(context.Background(), "bump(41)", nil)
	if err != nil {
		t.Fatal(err)
	}
	if value.Int() != 42 {
		t.Fatalf("got %d", value.Int())
	}
}

func TestSmokeCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	started := time.Now()
	_, err := Eval(ctx, `
n = 0
while True:
    n = n + 1
n
`, nil)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v does not match context.Canceled", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("cancellation took %v", elapsed)
	}
}

func TestSmokeTimeLimit(t *testing.T) {
	_, err := Eval(context.Background(), `
n = 0
while True:
    n = n + 1
n
`, nil, WithLimits(Limits{MaxDuration: 30 * time.Millisecond}))
	if !errors.Is(err, ErrTimeLimit) {
		t.Fatalf("error %v does not match ErrTimeLimit", err)
	}
}

func TestSmokeValueIterators(t *testing.T) {
	value, err := Eval(context.Background(), `{"a": 1, "b": [2, 3]}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for k, v := range value.Entries() {
		if v.Kind() == ListKind {
			sum := 0
			for item := range v.Elems() {
				sum += item.Int()
			}
			got[k.Str()] = sum
		} else {
			got[k.Str()] = v.Int()
		}
	}
	if got["a"] != 1 || got["b"] != 5 {
		t.Fatalf("got %v", got)
	}
	if item, ok := value.Get("b"); !ok || item.Len() != 2 {
		t.Fatalf("Get(b) = %v, %v", item, ok)
	}
}

func TestSmokeTypeCheck(t *testing.T) {
	_, err := Compile(`x: int = "nope"`, WithTypeCheck())
	var typeErr *TypeCheckError
	if !errors.As(err, &typeErr) {
		t.Fatalf("error is %T (%v), want *TypeCheckError", err, err)
	}
	if len(typeErr.Diagnostics) == 0 {
		t.Fatalf("no structured diagnostics; summary %q", typeErr.Error())
	}
	if rendered := typeErr.Render(DiagnosticConcise, false); rendered == "" {
		t.Fatal("empty concise rendering")
	}
}

func TestSmokeMountAndFS(t *testing.T) {
	dir := t.TempDir()
	if err := writeFile(dir+"/input.txt", "from-mount"); err != nil {
		t.Fatal(err)
	}
	mount, err := NewMountDir("/workspace", dir, WithMode(MountReadOnly))
	if err != nil {
		t.Fatal(err)
	}
	defer mount.Close()
	got, err := EvalAs[string](context.Background(), `
from pathlib import Path
Path("/workspace/input.txt").read_text()
`, nil, WithMount(mount))
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-mount" {
		t.Fatalf("got %q", got)
	}
}
