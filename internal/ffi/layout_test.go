package ffi

import (
	"testing"
	"unsafe"
)

// TestABIStructLayouts mirrors the layout assertions in
// crates/monty-ffi/src/lib.rs (mod layout_tests). When a #[repr(C)] struct
// changes on one side only, exactly one of the two suites fails.
func TestABIStructLayouts(t *testing.T) {
	check := func(name string, got, want uintptr) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}

	check("sizeof(Str)", unsafe.Sizeof(Str{}), 16)
	check("sizeof(Bytes)", unsafe.Sizeof(Bytes{}), 16)

	var raw RawValue
	check("sizeof(RawValue)", unsafe.Sizeof(raw), 48)
	check("offsetof(RawValue.Int)", unsafe.Offsetof(raw.Int), 8)
	check("offsetof(RawValue.Float)", unsafe.Offsetof(raw.Float), 16)
	check("offsetof(RawValue.Ptr)", unsafe.Offsetof(raw.Ptr), 24)
	check("offsetof(RawValue.Len)", unsafe.Offsetof(raw.Len), 32)
	check("offsetof(RawValue.Handle)", unsafe.Offsetof(raw.Handle), 40)

	check("sizeof(RawPair)", unsafe.Sizeof(RawPair{}), 96)

	var limits Limits
	check("sizeof(Limits)", unsafe.Sizeof(limits), 96)
	check("offsetof(Limits.CancelToken)", unsafe.Offsetof(limits.CancelToken), 88)

	var fast RunFastOutput
	check("sizeof(RunFastOutput)", unsafe.Sizeof(fast), 104+FastScratchCap)
	check("offsetof(RunFastOutput.Value)", unsafe.Offsetof(fast.Value), 16)
	check("offsetof(RunFastOutput.Bytes)", unsafe.Offsetof(fast.Bytes), 64)
	check("offsetof(RunFastOutput.Print)", unsafe.Offsetof(fast.Print), 80)
	check("offsetof(RunFastOutput.Error)", unsafe.Offsetof(fast.Error), 96)
	check("offsetof(RunFastOutput.Scratch)", unsafe.Offsetof(fast.Scratch), 104)

	var snap ProgressSnapshot
	check("sizeof(ProgressSnapshot)", unsafe.Sizeof(snap), 176)
	check("offsetof(ProgressSnapshot.Name)", unsafe.Offsetof(snap.Name), 16)

	var snapOut ProgressSnapshotOutput
	check("sizeof(ProgressSnapshotOutput)", unsafe.Sizeof(snapOut), 224)
	check("offsetof(ProgressSnapshotOutput.Repl)", unsafe.Offsetof(snapOut.Repl), 8)
	check("offsetof(ProgressSnapshotOutput.Print)", unsafe.Offsetof(snapOut.Print), 24)
	check("offsetof(ProgressSnapshotOutput.PrintFlags)", unsafe.Offsetof(snapOut.PrintFlags), 40)
	check("offsetof(ProgressSnapshotOutput.Snapshot)", unsafe.Offsetof(snapOut.Snapshot), 48)

	check("sizeof(RunJSONOutput)", unsafe.Sizeof(RunJSONOutput{}), 48)
	check("sizeof(HostFunctionOutput)", unsafe.Sizeof(HostFunctionOutput{}), 80)
	check("sizeof(FutureResult)", unsafe.Sizeof(FutureResult{}), 88)
	check("sizeof(RunHostArgs)", unsafe.Sizeof(RunHostArgs{}), 88)

	var feed ReplFeedArgs
	check("sizeof(ReplFeedArgs)", unsafe.Sizeof(feed), 128)
	check("offsetof(ReplFeedArgs.MaxDurationNanos)", unsafe.Offsetof(feed.MaxDurationNanos), 48)
	check("offsetof(ReplFeedArgs.HostNames)", unsafe.Offsetof(feed.HostNames), 64)

	check("sizeof(MountOutput)", unsafe.Sizeof(MountOutput{}), 64)
	check("sizeof(Date)", unsafe.Sizeof(Date{}), 8)
	check("sizeof(DateTime)", unsafe.Sizeof(DateTime{}), 40)
	check("sizeof(TimeDelta)", unsafe.Sizeof(TimeDelta{}), 12)
	check("sizeof(TimeZone)", unsafe.Sizeof(TimeZone{}), 24)
}
