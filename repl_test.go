package monty

import (
	"bytes"
	"context"
	"reflect"
	"testing"
)

func TestReplStateCallAndDumpLoad(t *testing.T) {
	ctx := context.Background()
	repl, err := NewRepl()
	if err != nil {
		t.Fatal(err)
	}
	defer repl.Close() //nolint:errcheck // best-effort cleanup in test

	value, err := repl.FeedRun(ctx, "x = 10\nx + y", Inputs{"y": Int(5)})
	if err != nil {
		t.Fatal(err)
	}
	if value.Int() != 15 {
		t.Fatalf("first value = %s, want 15", value)
	}

	if _, err := repl.FeedRun(ctx, "def add(a, b):\n    return a + b\n", nil); err != nil {
		t.Fatal(err)
	}
	if !repl.HasFunction("add") {
		t.Fatal("expected add to be registered")
	}
	names, err := repl.FunctionNames()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"add"}) {
		t.Fatalf("function names = %v, want [add]", names)
	}
	sum, err := repl.Call(ctx, "add", Int(2), Int(3))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Int() != 5 {
		t.Fatalf("sum = %s, want 5", sum)
	}

	snapshot, err := repl.Dump()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadRepl(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Close() //nolint:errcheck // best-effort cleanup in test
	loadedValue, err := loaded.FeedRun(ctx, "x + 1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if loadedValue.Int() != 11 {
		t.Fatalf("loaded value = %s, want 11", loadedValue)
	}
}

func TestReplStdoutAndContinuationMode(t *testing.T) {
	repl, err := NewRepl()
	if err != nil {
		t.Fatal(err)
	}
	defer repl.Close() //nolint:errcheck // best-effort cleanup in test

	var out bytes.Buffer
	if _, err := repl.FeedRun(context.Background(), `print("hello")`, nil, WithStdout(&out)); err != nil {
		t.Fatal(err)
	}
	if out.String() != "hello\n" {
		t.Fatalf("stdout = %q, want hello newline", out.String())
	}
	if got, err := DetectReplContinuationMode("1 + 1"); err != nil || got != ReplComplete {
		t.Fatalf("complete mode = %s, err = %v", got, err)
	}
	if got, err := DetectReplContinuationMode("(1 +"); err != nil || got != ReplIncompleteImplicit {
		t.Fatalf("implicit mode = %s, err = %v", got, err)
	}
	if got, err := DetectReplContinuationMode("if True:\n"); err != nil || got != ReplIncompleteBlock {
		t.Fatalf("block mode = %s, err = %v", got, err)
	}
}
