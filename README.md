# monty-go

> **Experimental:** This binding is built and maintained by coding agents with the goal of full parity with the [Python bindings](https://github.com/pydantic/monty/tree/main/python).

Go bindings for [Pydantic Monty](https://github.com/pydantic/monty), a sandboxed, snapshotable Python interpreter written in Rust.

The binding is purego-first: Go loads a small Rust `cdylib` and drives Monty's start/resume model itself. No cgo is required for consumers.

- **Full primitive coverage** — every Monty value kind, exception tracebacks with structured frames, type-check diagnostics in nine render formats, resource limits, filesystem mounts, OS-call interception, async futures, REPL sessions, and snapshot persistence.
- **Idiomatic Go** — `inputs any` with struct tags, generic `RunAs[T]` / `As[T]` / `Value.Decode`, Go 1.23 iterators over Python containers, `io.Writer` print streaming, `fs.FS` filesystem mounting, typed errors with `errors.Is/As`, and real `context.Context` cancellation that interrupts running Python.

## Install

```bash
go get github.com/joeychilson/monty
```

Release builds include a compressed `libmonty_ffi` shared library for each supported platform, extracted to the user cache and loaded automatically at runtime. Supported platforms: Linux (amd64, arm64) and macOS (amd64, arm64).

## Quick start

```go
prog, err := monty.Compile("x * x + y * y", monty.WithInputs("x", "y"))
if err != nil {
    log.Fatal(err)
}
defer prog.Close()

type Coords struct{ X, Y int } // fields map to snake_case input names
sum, err := monty.RunAs[int](ctx, prog, Coords{X: 3, Y: 4}) // 25
```

One-shot evaluation compiles, runs, and frees the program in a single FFI hop:

```go
greeting, err := monty.EvalAs[string](ctx, `"hello " + name`, map[string]any{"name": "go"})
```

## Host functions

Python code calls back into Go through registered functions. Arguments bind
to struct fields (kwargs style) or positional parameters, results convert
back automatically, and `PythonStub` generates typing stubs for the built-in
type checker:

```go
score := monty.MustFunction("fetch_score",
    func(ctx context.Context, player string) (int, error) {
        return lookupScore(ctx, player)
    },
    monty.WithParams("player"),
    monty.WithAsync(), // asyncio.gather branches run concurrently in goroutines
    monty.WithDoc("Fetch a player's score."),
)

prog, err := monty.Compile(`
import asyncio

async def main():
    a, b = await asyncio.gather(fetch_score("ada"), fetch_score("grace"))
    return a + b

await main()
`, monty.WithFunctions(score), monty.WithTypeCheck())
```

Returning a `*monty.Exception` (see `monty.Errorf`) raises that exception
type inside Python; other errors raise `RuntimeError`; panics are contained.

## Errors, limits, and cancellation

Failures are typed: `*monty.SyntaxError` from Compile, `*monty.TypeCheckError`
with structured diagnostics, and `*monty.ExecError` with the Python exception
type, message, and full traceback frames:

```go
_, err := monty.Eval(ctx, "1 / 0", nil)
var execErr *monty.ExecError
if errors.As(err, &execErr) {
    fmt.Println(execErr.Type)                          // ZeroDivisionError
    fmt.Println(execErr.Render(monty.FormatTraceback)) // full Python traceback
}
```

Resource limits bound allocations, memory, recursion, and time; limit
failures match `monty.ErrTimeLimit`, `monty.ErrMemoryLimit`, and
`monty.ErrRecursionLimit` via `errors.Is`. A context deadline becomes a hard
interpreter limit, and plain `context.WithCancel` cancellation interrupts
running Python at the next statement boundary:

```go
_, err := monty.Eval(ctx, code, nil, monty.WithLimits(monty.Limits{
    MaxDuration: 50 * time.Millisecond,
    MaxMemory:   16 << 20,
}))
```

## Filesystem and OS access

The sandbox performs no I/O itself. Grant access explicitly:

```go
// Host directory mounts (overlay by default: writes stay in memory).
mount, err := monty.NewMountDir("/workspace", "/safe/host/dir", monty.WithMode(monty.MountReadOnly))
defer mount.Close()
_, err = prog.Run(ctx, nil, monty.WithMount(mount))

// Serve read-only files from any fs.FS — embed.FS, os.DirFS, fstest.MapFS.
//go:embed data
var dataFS embed.FS
_, err = prog.Run(ctx, nil, monty.WithFS("/data", dataFS))

// Answer anything else (os.getenv, date.today, custom stat results) in Go.
_, err = prog.Run(ctx, nil, monty.WithOSHandler(
    func(ctx context.Context, fn monty.OSFunction, args []monty.Value, kwargs map[string]monty.Value) (any, error) {
        if fn == monty.OSGetenv && args[0].Str() == "HOME" {
            return "/home/sandbox", nil
        }
        return nil, monty.ErrNotHandled
    }))
```

Print output streams live to `io.Writer`s via `monty.WithStdout` /
`monty.WithStderr`.

## Pause, snapshot, resume

`Program.Start` returns a `*monty.Run`: a pausable execution that surfaces
every external interaction as a typed interrupt — the foundation for durable,
agent-style workflows:

```go
run, err := prog.Start(ctx, inputs)
defer run.Close()

for run.Paused() {
    switch req := run.Pending().(type) {
    case *monty.Call: // host function or OS call (req.OS)
        err = req.Return(ctx, answer) // or req.Raise, req.Defer, req.NotHandled
    case *monty.NameLookup:
        err = req.Return(ctx, monty.ExternalFunction(req.Name)) // or req.Undefined
    case *monty.Gather: // deferred futures Python is awaiting
        err = req.Resolve(ctx, monty.FutureValue(id, result))
    }
}
value, err := run.Result()
```

A paused run serializes with `run.Dump()` and resumes later — in another
process — with `monty.LoadRun` (dispatch options are re-provided on load).
Registered functions, mounts, and OS handlers are auto-dispatched between
pauses, so only interrupts you need to see surface.

## REPL sessions

`monty.REPL` keeps Python state alive across snippets, with per-snippet
host functions, mounts, time budgets, and accumulated type checking:

```go
repl, err := monty.NewREPL(monty.WithTypeCheck())
defer repl.Close()

_, err = repl.Eval(ctx, "x = 40", nil)
v, err := repl.Eval(ctx, "x + 2", nil) // 42
```

Sessions serialize with `repl.Dump()` / `monty.LoadREPL`. A snippet started
with `repl.Start` is a full `*monty.Run` — including mid-snippet snapshots
restored with `monty.LoadREPLRun`.

## Dataclasses

Bind Go structs to Python dataclasses for round-tripping with class identity:

```go
type User struct{ ID int; Name string }
userClass, err := monty.DataclassFor[User]("User", monty.WithFrozen())
prog, err := monty.Compile("user.name", monty.WithInputs("user"), monty.WithDataclasses(userClass))
```

## Build from source

```bash
make embed-lib   # cargo build --release -p monty-ffi + embed the library
make test
make lint
```

The Go loader searches, in order:

- `$MONTY_GO_LIB` (must be an absolute path to an existing file)
- `crates/monty-ffi/target/release/libmonty_ffi.{dylib,so}`
- `target/release/libmonty_ffi.{dylib,so}`
- the repository root: `libmonty_ffi.{dylib,so}`
- embedded `internal/ffi/lib/$GOOS_$GOARCH/libmonty_ffi.{dylib,so}.gz`

Use `-tags monty_noembed` to build without the embedded fallback. The loader
verifies the library's ABI revision at startup and reports a clear error for
stale builds.

The embedded libraries refresh themselves: when a change touching the Rust
source lands on `main`, the "Refresh Embedded FFI Libraries" workflow rebuilds
all four platforms and commits the updated assets. The release workflow
refuses to publish if the Rust source changed after the last refresh, and CI
fails if the committed asset for its platform reports a stale ABI revision.
The Rust toolchain is pinned once in `rust-toolchain.toml`.

## Benchmarks

Side-by-side comparison against the Python `pydantic-monty` binding using
identical source snippets, inputs, and result checks. Run locally with
`make bench-compare`; the script and setup live in [`benchmarks/`](benchmarks/).

| benchmark | go | python | go vs python |
| --- | ---: | ---: | ---: |
| ArithmeticRun | 1.38 µs | 1.18 µs | 1.17x slower |
| ArithmeticCompileRun | 3.60 µs | 3.50 µs | 1.03x slower |
| OrderSummaryRun | 10.04 µs | 8.14 µs | 1.23x slower |
| OrderSummaryCompileRun | 36.37 µs | 26.32 µs | 1.38x slower |
| OrderSummaryJSON | 8.98 µs | 9.76 µs | 1.09x faster |
| StringNormalizationRun | 4.92 µs | 4.40 µs | 1.12x slower |
| RecordsResult100 | 119.43 µs | 95.41 µs | 1.25x slower |
| HostFunctionBatch | 18.41 µs | 12.19 µs | 1.51x slower |
| HostFunctionStructKwargs | 3.08 µs | 2.30 µs | 1.34x slower |

Measured on darwin/arm64 (Apple M2) with `go test -benchtime=1s` and 5 Python
samples (min of samples reported).

## License

MIT
