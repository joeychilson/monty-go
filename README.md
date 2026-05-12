# monty-go

Go bindings for [Pydantic Monty](https://github.com/pydantic/monty), a sandboxed, snapshotable Python interpreter written in Rust.

This binding is purego-first: Go loads a small Rust `cdylib` and drives Monty's `start/resume` model itself. No cgo is required for consumers. Registered Go functions use the public start/resume flow by default, with an internal callback fast path for simple raw-compatible functions.

## Install

```bash
go get github.com/joeychilson/monty
```

Release builds include a compressed `libmonty_ffi` shared library for each supported platform. At runtime, Monty extracts the matching library into the user cache and loads it automatically. To override the bundled library, set `MONTY_GO_LIB` to an absolute path or place a local build on one of the development search paths below.

Supported platforms: Linux (amd64, arm64) and macOS (amd64, arm64). Windows is not built or tested.

## Build

```bash
cargo build --release -p monty-ffi
scripts/install-ffi-library.sh
go test ./...
go vet ./...
```

The Go loader searches, in order:

- `$MONTY_GO_LIB` (absolute path to the library)
- `target/release/libmonty_ffi.{dylib,so}`
- `crates/monty-ffi/target/release/libmonty_ffi.{dylib,so}`
- embedded `internal/ffi/lib/$GOOS_$GOARCH/libmonty_ffi.{dylib,so}.gz`

Use `-tags monty_noembed` to build without the embedded fallback.

When the Rust FFI changes, run the "Refresh Embedded FFI Libraries" workflow before tagging a release. The release workflow rebuilds the same assets and fails if the committed embedded libraries are stale.

## Example

```go
prog, err := monty.Compile("x * x + y * y", monty.WithInputs("x", "y"))
if err != nil {
	log.Fatal(err)
}
defer prog.Close()

result, err := monty.RunAs[int](
	context.Background(),
	prog,
	monty.InputsOf(Coords{X: 3, Y: 4}),
)
```

See `examples/` for host functions, snapshot resume, and async future resolution.

```go
prog, err := monty.Compile(`
from pathlib import Path
Path("/workspace/input.txt").read_text()
`)
if err != nil {
	log.Fatal(err)
}
defer prog.Close()

text, err := monty.RunAs[string](
	context.Background(),
	prog,
	nil,
	monty.WithMount("/workspace", "/safe/host/dir", monty.MountReadOnly),
)
```

Overlay mounts use Monty's copy-on-write filesystem backend:

```go
mount, err := monty.NewMountDir("/workspace", "/safe/host/dir")
if err != nil {
	log.Fatal(err)
}
defer mount.Close()

_, err = prog.Run(context.Background(), nil, monty.WithMountDir(mount))
```

Stateful REPL sessions preserve globals between snippets:

```go
repl, err := monty.NewRepl()
if err != nil {
	log.Fatal(err)
}
defer repl.Close()

value, err := repl.FeedRun(context.Background(), "x = 40\nx + 2", nil)
```

Values and completed runs can be serialized with Monty's natural JSON form:

```go
jsonBytes, err := prog.RunJSON(context.Background(), nil)
```

## License

MIT
