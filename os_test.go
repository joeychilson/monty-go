package monty

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWithMountReadText(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi from mount"), 0o600); err != nil {
		t.Fatal(err)
	}
	program, err := Compile(`
from pathlib import Path
Path("/mnt/hello.txt").read_text()
`)
	if err != nil {
		t.Fatal(err)
	}
	defer program.Close()

	result, err := RunAs[string](context.Background(), program, nil, WithMount("/mnt", dir, MountReadOnly))
	if err != nil {
		t.Fatal(err)
	}
	if result != "hi from mount" {
		t.Fatalf("result = %q, want mounted file contents", result)
	}
}

func TestWithMountReadOnlyRejectsWrite(t *testing.T) {
	program, err := Compile(`
from pathlib import Path
Path("/mnt/out.txt").write_text("nope")
`)
	if err != nil {
		t.Fatal(err)
	}
	defer program.Close()

	_, err = program.Run(context.Background(), nil, WithMount("/mnt", t.TempDir(), MountReadOnly))
	if err == nil {
		t.Fatal("expected write to fail")
	}
	if !strings.Contains(err.Error(), "PermissionError") {
		t.Fatalf("error = %q, want PermissionError", err)
	}
}

func TestWithMountOverlayAndWriteLimit(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	program, err := Compile(`
from pathlib import Path
p = Path("/mnt/out.txt")
(p.write_text("overlay"), p.read_text(), Path("/mnt/base.txt").read_text())
`)
	if err != nil {
		t.Fatal(err)
	}
	defer program.Close()

	value, err := program.Run(context.Background(), nil, WithMountOptions("/mnt", dir))
	if err != nil {
		t.Fatal(err)
	}
	items := value.Items()
	if got := items[0].Int(); got != len("overlay") {
		t.Fatalf("write_text return = %d, want %d", got, len("overlay"))
	}
	if got := items[1].Str(); got != "overlay" {
		t.Fatalf("overlay read = %q, want overlay", got)
	}
	if got := items[2].Str(); got != "base" {
		t.Fatalf("base read = %q, want base", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("host out.txt exists after overlay run: %v", err)
	}

	limitProgram, err := Compile(`
from pathlib import Path
Path("/mnt/out.txt").write_text("too much")
`)
	if err != nil {
		t.Fatal(err)
	}
	defer limitProgram.Close()
	_, err = limitProgram.Run(context.Background(), nil, WithMountOptions("/mnt", dir, WithWriteBytesLimit(3)))
	if err == nil {
		t.Fatal("expected write limit error")
	}
	if !strings.Contains(err.Error(), "disk write limit") {
		t.Fatalf("error = %q, want write limit", err)
	}
}

func TestMountDirPersistsOverlayState(t *testing.T) {
	mount, err := NewMountDir("/mnt", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer mount.Close() //nolint:errcheck // best-effort cleanup in test

	writer, err := Compile(`
from pathlib import Path
Path("/mnt/session.txt").write_text("persisted")
`)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Run(context.Background(), nil, WithMountDir(mount)); err != nil {
		t.Fatal(err)
	}

	reader, err := Compile(`
from pathlib import Path
Path("/mnt/session.txt").read_text()
`)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	result, err := RunAs[string](context.Background(), reader, nil, WithMountDir(mount))
	if err != nil {
		t.Fatal(err)
	}
	if result != "persisted" {
		t.Fatalf("result = %q, want persisted overlay state", result)
	}
}

func TestWithOSHandler(t *testing.T) {
	program, err := Compile(`
from pathlib import Path
Path("/virtual/value.txt").read_text()
`)
	if err != nil {
		t.Fatal(err)
	}
	defer program.Close()

	handler := func(ctx context.Context, request OSRequest) (Value, error) {
		if err := ctx.Err(); err != nil {
			return Value{}, err
		}
		if request.Function != "Path.read_text" {
			return Value{}, &Error{Type: "OSError", Message: "unexpected OS function"}
		}
		return Str("handled"), nil
	}
	result, err := RunAs[string](context.Background(), program, nil, WithOSHandler(handler))
	if err != nil {
		t.Fatal(err)
	}
	if result != "handled" {
		t.Fatalf("result = %q, want handler result", result)
	}
}
