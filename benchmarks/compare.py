#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.10"
# dependencies = [
#   "pydantic-monty",
# ]
# ///
"""Run matching Go and pydantic-monty benchmarks and print a ratio table."""

import argparse
import gc
import json
import re
import subprocess
import sys
import time
from pathlib import Path


ARITHMETIC_CODE = "x * x + y * y"
ARITHMETIC_INPUTS = {"x": 3, "y": 4}

ORDER_SUMMARY_CODE = """
subtotal = 0
quantity = 0
category_totals = {}
for item in items:
    line_total = item["price_cents"] * item["quantity"]
    subtotal += line_total
    quantity += item["quantity"]
    category = item["category"]
    category_totals[category] = category_totals.get(category, 0) + line_total

discount = 0
if coupon == "SUMMER10" and subtotal >= 5000:
    discount = subtotal // 10
elif coupon == "FREESHIP" and quantity >= 3:
    discount = shipping_cents

tax = (subtotal - discount) * tax_basis_points // 10000
{
    "subtotal_cents": subtotal,
    "discount_cents": discount,
    "tax_cents": tax,
    "total_cents": subtotal - discount + tax + shipping_cents,
    "category_totals": category_totals,
}
"""

ORDER_INPUT_NAMES = ["items", "coupon", "shipping_cents", "tax_basis_points"]
ORDER_INPUTS = {
    "items": [
        {"sku": "sku-001", "category": "books", "quantity": 2, "price_cents": 1299},
        {"sku": "sku-002", "category": "kitchen", "quantity": 5, "price_cents": 499},
        {"sku": "sku-003", "category": "books", "quantity": 1, "price_cents": 3499},
        {"sku": "sku-004", "category": "games", "quantity": 3, "price_cents": 899},
    ],
    "coupon": "SUMMER10",
    "shipping_cents": 799,
    "tax_basis_points": 825,
}

STRING_NORMALIZATION_CODE = """
clean = message.lower()
for ch in "-_,.;:/":
    clean = clean.replace(ch, " ")

tokens = []
for token in clean.split():
    if len(token) > 2:
        tokens.append(token)

"|".join(tokens)
"""
STRING_INPUTS = {"message": "Invoice-8841, PAID; customer: ACME_Corp / region: NORTH"}
STRING_RESULT = "invoice|8841|paid|customer|acme|corp|region|north"

RECORDS_CODE = """
records = []
for i in range(count):
    records.append({
        "id": i,
        "score": (i * 37 + seed) % 1000,
        "active": i % 3 != 0,
        "label": "user-" + str(i),
    })
records
"""
RECORDS_INPUTS = {"count": 100, "seed": 17}

HOST_FUNCTION_CODE = """
total = 0
for n in numbers:
    total += score(n)
total
"""
HOST_INPUTS = {"numbers": list(range(1, 17))}

ORDER_JSON_NEEDLE = '"total_cents":11798'


class BenchCase:
    def __init__(self, name, setup, run, check):
        self.name = name
        self.setup = setup
        self.run = run
        self.check = check


def check_equal(want):
    def check(got):
        if got != want:
            raise AssertionError("got %r, want %r" % (got, want))

    return check


def check_order_summary(result):
    if result["total_cents"] != 11798:
        raise AssertionError("unexpected order total: %r" % (result,))


def check_order_json(result):
    if ORDER_JSON_NEEDLE not in result:
        raise AssertionError("unexpected JSON result: %s" % result)


def check_records(result):
    if len(result) != 100 or result[99]["score"] != 680:
        raise AssertionError("unexpected records result: %r" % (result[-1:] if result else result,))


def build_cases(pydantic_monty):
    def monty(code, inputs=None):
        return pydantic_monty.Monty(code, inputs=inputs)

    def score(value):
        return value * value + 7

    def measure(**kwargs):
        return kwargs["x"] * kwargs["x"] + kwargs["y"] * kwargs["y"]

    return [
        BenchCase(
            "BenchmarkCompareArithmeticRun",
            lambda: monty(ARITHMETIC_CODE, ["x", "y"]),
            lambda program: program.run(inputs=ARITHMETIC_INPUTS),
            check_equal(25),
        ),
        BenchCase(
            "BenchmarkCompareArithmeticCompileRun",
            lambda: None,
            lambda _state: monty(ARITHMETIC_CODE, ["x", "y"]).run(inputs=ARITHMETIC_INPUTS),
            check_equal(25),
        ),
        BenchCase(
            "BenchmarkCompareOrderSummaryRun",
            lambda: monty(ORDER_SUMMARY_CODE, ORDER_INPUT_NAMES),
            lambda program: program.run(inputs=ORDER_INPUTS),
            check_order_summary,
        ),
        BenchCase(
            "BenchmarkCompareOrderSummaryCompileRun",
            lambda: None,
            lambda _state: monty(ORDER_SUMMARY_CODE, ORDER_INPUT_NAMES).run(inputs=ORDER_INPUTS),
            check_order_summary,
        ),
        BenchCase(
            "BenchmarkCompareOrderSummaryJSON",
            lambda: monty(ORDER_SUMMARY_CODE, ORDER_INPUT_NAMES),
            lambda program: json.dumps(program.run(inputs=ORDER_INPUTS), separators=(",", ":")),
            check_order_json,
        ),
        BenchCase(
            "BenchmarkCompareStringNormalizationRun",
            lambda: monty(STRING_NORMALIZATION_CODE, ["message"]),
            lambda program: program.run(inputs=STRING_INPUTS),
            check_equal(STRING_RESULT),
        ),
        BenchCase(
            "BenchmarkCompareRecordsResult100",
            lambda: monty(RECORDS_CODE, ["count", "seed"]),
            lambda program: program.run(inputs=RECORDS_INPUTS),
            check_records,
        ),
        BenchCase(
            "BenchmarkCompareHostFunctionBatch",
            lambda: (monty(HOST_FUNCTION_CODE, ["numbers"]), {"score": score}),
            lambda state: state[0].run(inputs=HOST_INPUTS, external_functions=state[1]),
            check_equal(1608),
        ),
        BenchCase(
            "BenchmarkCompareHostFunctionStructKwargs",
            lambda: (monty("measure(x=x, y=y)", ["x", "y"]), {"measure": measure}),
            lambda state: state[0].run(inputs=ARITHMETIC_INPUTS, external_functions=state[1]),
            check_equal(25),
        ),
    ]


def measure_case(case, min_time_secs, repeats):
    state = case.setup()
    case.check(case.run(state))

    min_time_ns = int(min_time_secs * 1_000_000_000)
    loops = 1
    while True:
        elapsed = time_loop(case, state, loops)
        if elapsed >= min_time_ns:
            break
        loops *= 2

    samples = []
    for _ in range(repeats):
        gc.collect()
        elapsed = time_loop(case, state, loops)
        samples.append(elapsed / loops)
    return {"name": case.name, "loops": loops, "ns_per_op": min(samples)}


def time_loop(case, state, loops):
    start = time.perf_counter_ns()
    for _ in range(loops):
        case.run(state)
    return time.perf_counter_ns() - start


GO_BENCH_RE = re.compile(
    r"^(BenchmarkCompare\S+)-\d+\s+(\d+)\s+([0-9.]+)\s+ns/op"
    r"(?:\s+([0-9.]+)\s+B/op\s+([0-9.]+)\s+allocs/op)?"
)


def run_go_benchmarks(repo_root, benchtime):
    cmd = [
        "go",
        "test",
        "-run",
        "^$",
        "-bench",
        "^BenchmarkCompare",
        "-benchmem",
        "-benchtime",
        benchtime,
        ".",
    ]
    completed = subprocess.run(cmd, cwd=str(repo_root), text=True, capture_output=True)
    if completed.returncode != 0:
        sys.stderr.write(completed.stdout)
        sys.stderr.write(completed.stderr)
        raise SystemExit(completed.returncode)

    results = {}
    for line in completed.stdout.splitlines():
        match = GO_BENCH_RE.match(line)
        if not match:
            continue
        name, loops, ns_per_op, bytes_per_op, allocs_per_op = match.groups()
        results[name] = {
            "name": name,
            "loops": int(loops),
            "ns_per_op": float(ns_per_op),
            "bytes_per_op": float(bytes_per_op) if bytes_per_op is not None else None,
            "allocs_per_op": float(allocs_per_op) if allocs_per_op is not None else None,
        }
    return results


def run_python_benchmarks(min_time, repeats):
    try:
        import pydantic_monty
    except ImportError as exc:
        raise SystemExit(
            "pydantic_monty is not installed. Run this through uv so the inline dependency is installed:\n"
            "  uv run --script benchmarks/compare.py\n"
        ) from exc

    results = {}
    for case in build_cases(pydantic_monty):
        result = measure_case(case, min_time, repeats)
        results[result["name"]] = result
    return results


def format_duration(ns):
    if ns >= 1_000_000:
        return "%.2f ms" % (ns / 1_000_000)
    if ns >= 1_000:
        return "%.2f us" % (ns / 1_000)
    return "%.0f ns" % ns


def ratio_label(go_ns, python_ns):
    ratio = go_ns / python_ns
    if ratio < 1:
        return "%.2fx faster" % (1 / ratio)
    return "%.2fx slower" % ratio


def print_table(go_results, python_results):
    names = [case.name for case in build_cases(DummyMontyModule())]
    print("%-42s %12s %12s %15s" % ("benchmark", "go", "python", "go vs python"))
    print("-" * 85)
    for name in names:
        go = go_results.get(name)
        py = python_results.get(name)
        go_text = format_duration(go["ns_per_op"]) if go else "-"
        py_text = format_duration(py["ns_per_op"]) if py else "-"
        ratio = ratio_label(go["ns_per_op"], py["ns_per_op"]) if go and py else "-"
        print("%-42s %12s %12s %15s" % (name.replace("BenchmarkCompare", ""), go_text, py_text, ratio))


class DummyMontyModule:
    class Monty:
        def __init__(self, *args, **kwargs):
            pass


def parse_args():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go-benchtime", default="1s", help="value passed to go test -benchtime")
    parser.add_argument("--python-min-time", type=float, default=0.35, help="minimum seconds per Python sample")
    parser.add_argument("--python-repeats", type=int, default=5, help="Python samples per benchmark")
    parser.add_argument("--go-only", action="store_true", help="only run Go benchmarks")
    parser.add_argument("--python-only", action="store_true", help="only run Python benchmarks")
    return parser.parse_args()


def main():
    args = parse_args()
    if args.go_only and args.python_only:
        raise SystemExit("--go-only and --python-only cannot be used together")

    repo_root = Path(__file__).resolve().parents[1]
    go_results = {}
    python_results = {}

    if not args.python_only:
        go_results = run_go_benchmarks(repo_root, args.go_benchtime)
    if not args.go_only:
        python_results = run_python_benchmarks(args.python_min_time, args.python_repeats)

    print_table(go_results, python_results)


if __name__ == "__main__":
    main()
