package ffi

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// TestFindLibraryValidatesMontyGoLib guards §3.12: MONTY_GO_LIB must be an
// absolute path to an existing file, and a bad value yields a descriptive error
// rather than being handed straight to Dlopen.
func TestFindLibraryValidatesMontyGoLib(t *testing.T) {
	t.Setenv("MONTY_GO_LIB", "relative/path/libmonty_ffi.so")
	if _, err := findLibrary(); err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("relative MONTY_GO_LIB error = %v, want one mentioning absolute path", err)
	}

	t.Setenv("MONTY_GO_LIB", "/nonexistent/libmonty_ffi.so")
	if _, err := findLibrary(); err == nil || !strings.Contains(err.Error(), "not accessible") {
		t.Fatalf("missing MONTY_GO_LIB error = %v, want one mentioning not accessible", err)
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

	//nolint:gosec,noctx // G204: re-execs this test binary with a fixed filter, no untrusted input; the child needs no context
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

// loadableNonMontyLibrary returns the absolute path of a shared library that
// exists on disk, loads on this platform, but exports none of the mg_* symbols,
// skipping the test if none is available. The path must exist on disk because
// findLibrary now rejects a MONTY_GO_LIB that fails os.Stat (§3.12); macOS
// system libraries live in the dyld shared cache and are not on disk, so this
// globs on-disk locations (Homebrew, the multiarch libc dirs) instead.
func loadableNonMontyLibrary(t *testing.T) string {
	t.Helper()
	var patterns []string
	switch runtime.GOOS {
	case "darwin":
		patterns = []string{"/opt/homebrew/lib/*.dylib", "/usr/local/lib/*.dylib"}
	case "linux":
		patterns = []string{"/lib/*/libc.so.6", "/usr/lib/*/libc.so.6", "/lib/libc.so.6", "/usr/lib/libc.so.6", "/lib64/libc.so.6"}
	default:
		t.Skipf("no known non-monty library to load on %s", runtime.GOOS)
	}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, candidate := range matches {
			if !filepath.IsAbs(candidate) {
				continue
			}
			if _, err := os.Stat(candidate); err != nil {
				continue
			}
			handle, err := purego.Dlopen(candidate, purego.RTLD_NOW|purego.RTLD_LOCAL)
			if err != nil {
				continue
			}
			_ = purego.Dlclose(handle) //nolint:errcheck // best-effort close of a probe handle
			return candidate
		}
	}
	t.Skipf("no on-disk non-monty library found for the stale-library test (patterns: %v)", patterns)
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
