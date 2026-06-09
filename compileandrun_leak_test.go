package monty

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestCompileAndRunFreesHandlesOnError guards the fix for CODE_REVIEW.md §2.1:
// CompileAndRun must free owned Rust value handles built for inputs whose Kind
// has no inline raw form (here a datetime) when the run fails before Rust reads
// the inputs. A syntax error makes mg_program_compile_run_fast_raw fail in
// MontyRun::new — before read_raw_values — so the handle is never consumed and
// the deferred freeOwnedRawValues(raw) is the only thing that reclaims it.
//
// There is no Rust-side handle counter to assert against, so this drives the
// leak path many times under GC pressure: a premature collection (missing
// keep-alive) or a double-free regression would crash or corrupt here, and the
// fix's freeing path is exercised every iteration.
func TestCompileAndRunFreesHandlesOnError(t *testing.T) {
	ctx := context.Background()
	stop := hammerGC(t)
	defer stop()

	const iterations = 2000
	for i := range iterations {
		_, err := CompileAndRun(ctx, "syntax error(", Inputs{
			"t": DateTime(MontyDateTimeFromTime(time.Now())),
		})
		if err == nil {
			t.Fatalf("iteration %d: expected a compile error, got nil", i)
		}
		if !strings.Contains(err.Error(), "Error") {
			t.Fatalf("iteration %d: unexpected error %v", i, err)
		}
	}
}

// TestCompileAndRunOwnedHandleSuccess is the companion guard: when the run
// succeeds, Rust consumes the owned handle in place (nulling its slot), so the
// deferred freeOwnedRawValues(raw) added by §2.1 must be a no-op rather than a
// double-free. Looping with an owned-handle (datetime) input under GC pressure
// would surface a double-free or premature collection as a crash or wrong
// result.
func TestCompileAndRunOwnedHandleSuccess(t *testing.T) {
	ctx := context.Background()
	stop := hammerGC(t)
	defer stop()

	const iterations = 2000
	for i := range iterations {
		got, err := CompileAndRunAs[int](ctx, "t.year * 10000 + t.month * 100 + t.day", Inputs{
			"t": DateTime(MontyDateTimeFromTime(time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC))),
		})
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if got != 20260609 {
			t.Fatalf("iteration %d: got %d, want 20260609", i, got)
		}
	}
}
