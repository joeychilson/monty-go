package monty

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestMountModes(t *testing.T) {
	ctx := context.Background()

	t.Run("read-only rejects writes", func(t *testing.T) {
		dir := t.TempDir()
		mount, err := NewMountDir("/data", dir, WithMode(MountReadOnly))
		if err != nil {
			t.Fatal(err)
		}
		defer mount.Close()
		_, err = Eval(ctx, `
from pathlib import Path
Path("/data/out.txt").write_text("nope")
`, nil, WithMount(mount))
		var execErr *ExecError
		if !errors.As(err, &execErr) || execErr.Type != "PermissionError" {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("read-write writes through", func(t *testing.T) {
		dir := t.TempDir()
		mount, err := NewMountDir("/data", dir, WithMode(MountReadWrite))
		if err != nil {
			t.Fatal(err)
		}
		defer mount.Close()
		_, err = Eval(ctx, `
from pathlib import Path
Path("/data/out.txt").write_text("written")
`, nil, WithMount(mount))
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(dir, "out.txt")) //nolint:gosec // test reads from its own temp dir
		if err != nil || string(data) != "written" {
			t.Fatalf("host file = %q, %v", data, err)
		}
	})

	t.Run("overlay keeps writes in memory across runs", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base"), 0o600); err != nil {
			t.Fatal(err)
		}
		mount, err := NewMountDir("/data", dir) // overlay by default
		if err != nil {
			t.Fatal(err)
		}
		defer mount.Close()
		if mount.Mode() != MountOverlay {
			t.Fatalf("default mode = %s", mount.Mode())
		}
		_, err = Eval(ctx, `
from pathlib import Path
Path("/data/new.txt").write_text("overlay")
`, nil, WithMount(mount))
		if err != nil {
			t.Fatal(err)
		}
		// Host directory untouched.
		if _, err := os.Stat(filepath.Join(dir, "new.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("overlay write leaked to host: %v", err)
		}
		// The same MountDir sees the overlay write in a later run.
		got, err := EvalAs[string](ctx, `
from pathlib import Path
Path("/data/new.txt").read_text() + "+" + Path("/data/base.txt").read_text()
`, nil, WithMount(mount))
		if err != nil || got != "overlay+base" {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("write limit", func(t *testing.T) {
		dir := t.TempDir()
		mount, err := NewMountDir("/data", dir, WithWriteLimit(8))
		if err != nil {
			t.Fatal(err)
		}
		defer mount.Close()
		_, err = Eval(ctx, `
from pathlib import Path
Path("/data/big.txt").write_text("0123456789abcdef")
`, nil, WithMount(mount))
		if err == nil {
			t.Fatal("write over the limit should fail")
		}
	})
}

func TestWithFS(t *testing.T) {
	ctx := context.Background()
	fsys := fstest.MapFS{
		"config.json": {Data: []byte(`{"k": 1}`), ModTime: time.Unix(1700000000, 0)},
		"docs/a.txt":  {Data: []byte("alpha")},
		"docs/b.txt":  {Data: []byte("beta")},
	}

	t.Run("read and stat", func(t *testing.T) {
		got, err := EvalAs[string](ctx, `
from pathlib import Path
p = Path("/embed/config.json")
text = p.read_text()
size = p.stat().st_size
f"{text}|{size}|{p.exists()}|{p.is_file()}"
`, nil, WithFS("/embed", fsys))
		if err != nil {
			t.Fatal(err)
		}
		if got != `{"k": 1}|8|True|True` {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("iterdir", func(t *testing.T) {
		got, err := EvalAs[[]string](ctx, `
from pathlib import Path
sorted(str(p) for p in Path("/embed/docs").iterdir())
`, nil, WithFS("/embed", fsys))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0] != "/embed/docs/a.txt" {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("writes denied", func(t *testing.T) {
		_, err := Eval(ctx, `
from pathlib import Path
Path("/embed/new.txt").write_text("x")
`, nil, WithFS("/embed", fsys))
		var execErr *ExecError
		if !errors.As(err, &execErr) || execErr.Type != "PermissionError" {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := Eval(ctx, `
from pathlib import Path
Path("/embed/nope.txt").read_text()
`, nil, WithFS("/embed", fsys))
		var execErr *ExecError
		if !errors.As(err, &execErr) || execErr.Type != "FileNotFoundError" {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("outside mount not handled", func(t *testing.T) {
		_, err := Eval(ctx, `
from pathlib import Path
Path("/elsewhere/x.txt").read_text()
`, nil, WithFS("/embed", fsys))
		var execErr *ExecError
		if !errors.As(err, &execErr) || execErr.Type != "PermissionError" {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestOSHandler(t *testing.T) {
	ctx := context.Background()
	handler := func(_ context.Context, fn OSFunction, args []Value, _ map[string]Value) (any, error) {
		switch fn {
		case OSGetenv:
			if args[0].Str() == "HOME" {
				return "/home/monty", nil
			}
			return nil, nil
		case OSDateToday:
			return Date{Year: 2026, Month: time.June, Day: 9}, nil
		default:
			return nil, ErrNotHandled
		}
	}
	got, err := EvalAs[string](ctx, `
import os
from datetime import date
f"{os.getenv('HOME')}|{date.today().isoformat()}"
`, nil, WithOSHandler(handler))
	if err != nil {
		t.Fatal(err)
	}
	if got != "/home/monty|2026-06-09" {
		t.Fatalf("got %q", got)
	}
}

func TestOSHandlerInStartMode(t *testing.T) {
	ctx := context.Background()
	prog, err := Compile(`
import os
os.getenv("USER")
`)
	if err != nil {
		t.Fatal(err)
	}
	defer prog.Close()
	handler := func(_ context.Context, fn OSFunction, _ []Value, _ map[string]Value) (any, error) {
		if fn == OSGetenv {
			return "pump-user", nil
		}
		return nil, ErrNotHandled
	}
	// The Start pump auto-dispatches OS calls when a handler is configured.
	run, err := prog.Start(ctx, nil, WithOSHandler(handler))
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	got, err := ResultAs[string](run)
	if err != nil || got != "pump-user" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestStatResultHelpers(t *testing.T) {
	mod := time.Unix(1700000000, 500_000_000)
	stat := FileStat(123, mod)
	value := stat.MontyValue()
	if value.Kind() != NamedTupleKind {
		t.Fatalf("kind = %s", value.Kind())
	}
	if size, ok := value.Attr("st_size"); !ok || size.Int64() != 123 {
		t.Fatalf("st_size = %v", size)
	}
	if mode, ok := value.Attr("st_mode"); !ok || mode.Int64() != 0o100_644 {
		t.Fatalf("st_mode = %o", mode.Int64())
	}
	if mtime, ok := value.Attr("st_mtime"); !ok || mtime.Float64() < 1.69e9 {
		t.Fatalf("st_mtime = %v", mtime)
	}
	dir := DirStat(mod)
	if dir.Mode != 0o040_755 {
		t.Fatalf("dir mode = %o", dir.Mode)
	}
}

func TestDataclassRoundTrip(t *testing.T) {
	ctx := context.Background()
	type User struct {
		ID   int
		Name string
	}
	userClass, err := DataclassFor[User]("User", WithFrozen())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(userClass.Stub(), "@dataclass(frozen=True)") {
		t.Fatalf("stub = %q", userClass.Stub())
	}

	prog, err := Compile(`
result = f"{user.name}#{user.id}"
result
`, WithInputs("user"), WithDataclasses(userClass))
	if err != nil {
		t.Fatal(err)
	}
	defer prog.Close()
	got, err := RunAs[string](ctx, prog, map[string]any{"user": User{ID: 7, Name: "ada"}})
	if err != nil {
		t.Fatal(err)
	}
	if got != "ada#7" {
		t.Fatalf("got %q", got)
	}

	// Explicit Wrap + decoding a dataclass output back into the Go type.
	wrapped, err := userClass.Wrap(User{ID: 9, Name: "grace"})
	if err != nil {
		t.Fatal(err)
	}
	value, err := Eval(ctx, "u", map[string]Value{"u": wrapped})
	if err != nil {
		t.Fatal(err)
	}
	if value.Kind() != DataclassKind || value.Dataclass().Name != "User" {
		t.Fatalf("value = %s", value)
	}
	var back User
	if err := value.Decode(&back); err != nil {
		t.Fatal(err)
	}
	if back != (User{ID: 9, Name: "grace"}) {
		t.Fatalf("back = %+v", back)
	}
}
