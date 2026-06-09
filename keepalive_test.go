package monty

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// hammerGC spins a sibling goroutine that repeatedly forces a garbage
// collection until the returned stop function is called. It is used by the
// keep-alive regression tests to maximise the chance that an unrooted FFI
// argument is reclaimed mid-call.
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

// TestCompileAndRunStringsSurviveGC guards the fix for CODE_REVIEW.md §1.1:
// ffi.Str must keep its backing string alive for the whole FFI call. The code
// and the string inputs are built fresh each iteration with fmt.Sprintf /
// strings.Repeat so they are heap-allocated rather than rodata-backed, and the
// only live reference to each is the one threaded into the Rust call. A sibling
// goroutine hammers the collector, so before the fix the code string (passed as
// a bare uintptr) could be reclaimed mid-run and the result came back wrong or
// errored. The result depends on both the code and the inputs surviving.
func TestCompileAndRunStringsSurviveGC(t *testing.T) {
	ctx := context.Background()
	stop := hammerGC(t)
	defer stop()

	const iterations = 3000
	for i := range iterations {
		// The unique comment forces a distinct backing array each time so the
		// source is never deduplicated to a constant.
		code := fmt.Sprintf("# iteration %d %s\nprefix + body", i, strings.Repeat("x", 48))
		prefix := fmt.Sprintf("prefix-%d-", i)
		body := strings.Repeat(fmt.Sprintf("b%d", i), 8)
		want := prefix + body

		got, err := CompileAndRunAs[string](ctx, code, Inputs{
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

// TestProgramRunStringsSurviveGC exercises the same keep-alive contract on the
// compiled Program.Run path, which reaches Rust through the hand-rolled
// syscall trampoline (ProgramRunFastRaw) rather than purego's registered
// wrappers. The trampoline degrades its pointer arguments to uintptr, so it
// relies on the explicit runtime.KeepAlive calls added for §1.1.
func TestProgramRunStringsSurviveGC(t *testing.T) {
	ctx := context.Background()
	program, err := Compile("prefix + body", WithInputs("prefix", "body"))
	if err != nil {
		t.Fatal(err)
	}
	defer program.Close()

	stop := hammerGC(t)
	defer stop()

	const iterations = 3000
	for i := range iterations {
		prefix := fmt.Sprintf("p-%d-", i)
		body := strings.Repeat(fmt.Sprintf("v%d", i), 8)
		want := prefix + body

		got, err := RunAs[string](ctx, program, Inputs{
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
