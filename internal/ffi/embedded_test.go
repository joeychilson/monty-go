package ffi

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ebitengine/purego"
)

func TestEmbeddedLibraryPath(t *testing.T) {
	path, err := embeddedLibraryPath(libraryFileName())
	if errors.Is(err, errEmbeddedLibraryUnavailable) {
		if os.Getenv("MONTY_GO_TEST_REQUIRE_EMBEDDED") != "" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != libraryFileName() {
		t.Fatalf("embedded library path base = %q, want %q", filepath.Base(path), libraryFileName())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		t.Fatalf("embedded library path is not a non-empty regular file: %s", path)
	}
	handle, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		t.Fatal(err)
	}
	if err := purego.Dlclose(handle); err != nil {
		t.Fatal(err)
	}
}
