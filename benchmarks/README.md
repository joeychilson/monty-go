# Monty Binding Benchmarks

These benchmarks compare this Go binding with the Python `pydantic-monty` binding
using the same source snippets, inputs, and result checks.

Run the Go-only benchmark suite:

```bash
make bench
```

Run the side-by-side comparison:

```bash
make bench-compare
```

`bench-compare` uses `uv run --script benchmarks/compare.py`. The script declares
`pydantic-monty` as an inline dependency, runs the matching Go benchmarks with
`go test -bench`, times the Python binding with `time.perf_counter_ns`, and prints
Go-vs-Python ratios.

For quicker local checks:

```bash
uv run --script benchmarks/compare.py --go-benchtime=100ms --python-min-time=0.1 --python-repeats=3
```
