package monty

import (
	"bytes"
	"context"
	"reflect"
	"testing"
	"time"
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

func TestReplRejectsUnsupportedRunOptions(t *testing.T) {
	ctx := context.Background()
	repl, err := NewRepl()
	if err != nil {
		t.Fatal(err)
	}
	defer repl.Close() //nolint:errcheck // best-effort cleanup in test

	cases := []struct {
		name string
		opt  RunOption
	}{
		{"WithLimits", WithLimits(Limits{MaxDuration: time.Second})},
		{"WithRunFunction", WithRunFunction(NewFunction("noop", func() int { return 0 }))},
		{"WithOSHandler", WithOSHandler(func(context.Context, OSRequest) (Value, error) { return None(), nil })},
		{"WithMount", WithMount("/data", t.TempDir(), MountReadOnly)},
	}
	for _, tc := range cases {
		t.Run("FeedRun/"+tc.name, func(t *testing.T) {
			if _, err := repl.FeedRun(ctx, "1 + 1", nil, tc.opt); err == nil {
				t.Fatalf("FeedRun with %s = nil error, want rejection", tc.name)
			}
		})
		t.Run("CallFunction/"+tc.name, func(t *testing.T) {
			if _, err := repl.CallFunction(ctx, "f", nil, tc.opt); err == nil {
				t.Fatalf("CallFunction with %s = nil error, want rejection", tc.name)
			}
		})
	}

	// WithStdout remains honored.
	var out bytes.Buffer
	if _, err := repl.FeedRun(ctx, `print("ok")`, nil, WithStdout(&out)); err != nil {
		t.Fatalf("FeedRun with WithStdout: %v", err)
	}
	if out.String() != "ok\n" {
		t.Fatalf("stdout = %q, want ok newline", out.String())
	}
}
