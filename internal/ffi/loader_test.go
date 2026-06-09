package ffi

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/ebitengine/purego"
)

// TestRegisterSymbolsMissingSymbolReturnsError covers the root cause of the
// "sync.Once poisoned into reporting success" bug: registerSymbols must report
// a missing symbol as a descriptive error instead of panicking. A panic would
// propagate out of loadOnce.Do, which still marks itself done, leaving loadErr
// nil so every later EnsureLoaded falsely reports success.
//
// RTLD_DEFAULT is a pseudo-handle whose search scope excludes the real monty
// library (loaded with RTLD_LOCAL), so none of the mg_* symbols resolve through
// it — a faithful, fully deterministic stand-in for a stale library.
func TestRegisterSymbolsMissingSymbolReturnsError(t *testing.T) {
	saved := lib
	t.Cleanup(func() { lib = saved })
	lib = purego.RTLD_DEFAULT

	const fakePath = "/stale/libmonty_ffi"
	err := registerSymbols(fakePath)
	if err == nil {
		t.Fatal("registerSymbols returned nil for a library missing every mg_* symbol")
	}
	got := err.Error()
	for _, want := range []string{"mg_bytes_free", "not found", "older than the Go binding", fakePath} {
		if !strings.Contains(got, want) {
			t.Errorf("error %q missing %q", got, want)
		}
	}
}

const (
	staleChildEnv           = "MONTY_GO_TEST_STALE_CHILD"
	staleChildSuccessMarker = "STALE_LIBRARY_STICKY_OK"
)

// TestEnsureLoadedStaleLibrarySticky reproduces the original bug end-to-end in a
// fresh process: point MONTY_GO_LIB at a loadable library that lacks the mg_*
// symbols and confirm EnsureLoaded returns the same descriptive error on every
// call (sticky) rather than crashing or reporting success on the second call.
//
// The child runs this same test with MONTY_GO_TEST_STALE_CHILD set; the parent
// only re-execs once it has confirmed a suitable library actually loads, so the
// test skips cleanly on platforms where it cannot find one.
func TestEnsureLoadedStaleLibrarySticky(t *testing.T) {
	if os.Getenv(staleChildEnv) == "1" {
		err1 := EnsureLoaded()
		err2 := EnsureLoaded()
		if err1 == nil || err2 == nil {
			t.Fatalf("expected a sticky non-nil load error, got err1=%v err2=%v", err1, err2)
		}
		if err1.Error() != err2.Error() {
			t.Fatalf("load error is not sticky across calls:\n  1: %v\n  2: %v", err1, err2)
		}
		if !strings.Contains(err1.Error(), "not found") {
			t.Fatalf("load error is not descriptive: %v", err1)
		}
		fmt.Println(staleChildSuccessMarker)
		return
	}

	libPath := loadableNonMontyLibrary(t)

	cmd := exec.Command(os.Args[0], "-test.run=^TestEnsureLoadedStaleLibrarySticky$", "-test.v")
	cmd.Env = append(env(),
		staleChildEnv+"=1",
		"MONTY_GO_LIB="+libPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child process failed (%v); want a clean sticky error, not a crash:\n%s", err, out)
	}
	if !strings.Contains(string(out), staleChildSuccessMarker) {
		t.Fatalf("child did not confirm sticky error behavior:\n%s", out)
	}
}

// loadableNonMontyLibrary returns the path of a shared library that loads on
// this platform but exports none of the mg_* symbols, skipping the test if none
// is available.
func loadableNonMontyLibrary(t *testing.T) string {
	t.Helper()
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{"/usr/lib/libSystem.B.dylib"}
	case "linux":
		candidates = []string{"libc.so.6", "libm.so.6", "libdl.so.2"}
	default:
		t.Skipf("no known non-monty library to load on %s", runtime.GOOS)
	}
	for _, candidate := range candidates {
		handle, err := purego.Dlopen(candidate, purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			continue
		}
		_ = purego.Dlclose(handle)
		return candidate
	}
	t.Skipf("none of %v could be loaded for the stale-library test", candidates)
	return ""
}

// env returns the current environment with the variables this test overrides
// stripped, so the appended values are unambiguous regardless of platform
// duplicate-key resolution.
func env() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "MONTY_GO_LIB=") || strings.HasPrefix(kv, staleChildEnv+"=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
