package monty

import (
	"bytes"
	"context"
	"testing"
)

const benchmarkArithmeticCode = `x * x + y * y`

const benchmarkOrderSummaryCode = `
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
`

const benchmarkStringNormalizationCode = `
clean = message.lower()
for ch in "-_,.;:/":
    clean = clean.replace(ch, " ")

tokens = []
for token in clean.split():
    if len(token) > 2:
        tokens.append(token)

"|".join(tokens)
`

const benchmarkRecordsCode = `
records = []
for i in range(count):
    records.append({
        "id": i,
        "score": (i * 37 + seed) % 1000,
        "active": i % 3 != 0,
        "label": "user-" + str(i),
    })
records
`

const benchmarkHostFunctionCode = `
total = 0
for n in numbers:
    total += score(n)
total
`

var (
	benchmarkCtx = context.Background()

	benchmarkArithmeticInputs = map[string]Value{
		"x": Int(3),
		"y": Int(4),
	}

	benchmarkOrderInputs = map[string]Value{
		"items": List(
			benchmarkOrderItem("sku-001", "books", 2, 1299),
			benchmarkOrderItem("sku-002", "kitchen", 5, 499),
			benchmarkOrderItem("sku-003", "books", 1, 3499),
			benchmarkOrderItem("sku-004", "games", 3, 899),
		),
		"coupon":           Str("SUMMER10"),
		"shipping_cents":   Int(799),
		"tax_basis_points": Int(825),
	}

	benchmarkStringInputs = map[string]Value{
		"message": Str("Invoice-8841, PAID; customer: ACME_Corp / region: NORTH"),
	}

	benchmarkRecordsInputs = map[string]Value{
		"count": Int(100),
		"seed":  Int(17),
	}

	benchmarkHostInputs = map[string]Value{
		"numbers": benchmarkIntList(16),
	}

	benchmarkOrderJSONNeedle = []byte(`"total_cents":11798`)
)

func BenchmarkCompareArithmeticRun(b *testing.B) {
	program := benchmarkCompile(b, benchmarkArithmeticCode, WithInputs("x", "y"))

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		result, err := RunAs[int](benchmarkCtx, program, benchmarkArithmeticInputs)
		if err != nil {
			b.Fatal(err)
		}
		if result != 25 {
			b.Fatal(result)
		}
	}
}

func BenchmarkCompareArithmeticCompileRun(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		result, err := EvalAs[int](benchmarkCtx, benchmarkArithmeticCode, benchmarkArithmeticInputs)
		if err != nil {
			b.Fatal(err)
		}
		if result != 25 {
			b.Fatal(result)
		}
	}
}

func BenchmarkCompareOrderSummaryRun(b *testing.B) {
	program := benchmarkCompile(
		b,
		benchmarkOrderSummaryCode,
		WithInputs("items", "coupon", "shipping_cents", "tax_basis_points"),
	)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		result, err := program.Run(benchmarkCtx, benchmarkOrderInputs)
		if err != nil {
			b.Fatal(err)
		}
		benchmarkExpectDictInt(b, result, "total_cents", 11798)
	}
}

func BenchmarkCompareOrderSummaryCompileRun(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		result, err := Eval(benchmarkCtx, benchmarkOrderSummaryCode, benchmarkOrderInputs)
		if err != nil {
			b.Fatal(err)
		}
		benchmarkExpectDictInt(b, result, "total_cents", 11798)
	}
}

func BenchmarkCompareOrderSummaryJSON(b *testing.B) {
	program := benchmarkCompile(
		b,
		benchmarkOrderSummaryCode,
		WithInputs("items", "coupon", "shipping_cents", "tax_basis_points"),
	)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		result, err := program.RunJSON(benchmarkCtx, benchmarkOrderInputs)
		if err != nil {
			b.Fatal(err)
		}
		if !bytes.Contains(result, benchmarkOrderJSONNeedle) {
			b.Fatalf("unexpected JSON result: %s", result)
		}
	}
}

func BenchmarkCompareStringNormalizationRun(b *testing.B) {
	program := benchmarkCompile(b, benchmarkStringNormalizationCode, WithInputs("message"))

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		result, err := RunAs[string](benchmarkCtx, program, benchmarkStringInputs)
		if err != nil {
			b.Fatal(err)
		}
		if result != "invoice|8841|paid|customer|acme|corp|region|north" {
			b.Fatal(result)
		}
	}
}

func BenchmarkCompareRecordsResult100(b *testing.B) {
	program := benchmarkCompile(b, benchmarkRecordsCode, WithInputs("count", "seed"))

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		result, err := program.Run(benchmarkCtx, benchmarkRecordsInputs)
		if err != nil {
			b.Fatal(err)
		}
		if result.Kind() != ListKind || result.Len() != 100 {
			b.Fatalf("unexpected result: %s len=%d", result.Kind(), result.Len())
		}
		benchmarkExpectDictInt(b, result.Index(99), "score", 680)
	}
}

func BenchmarkCompareHostFunctionBatch(b *testing.B) {
	score := MustFunction("score", func(value int) (int, error) {
		return value*value + 7, nil
	})
	program := benchmarkCompile(b, benchmarkHostFunctionCode, WithInputs("numbers"), WithFunctions(score))

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		result, err := RunAs[int](benchmarkCtx, program, benchmarkHostInputs)
		if err != nil {
			b.Fatal(err)
		}
		if result != 1608 {
			b.Fatal(result)
		}
	}
}

type benchmarkVector struct {
	X int `monty:"x"`
	Y int `monty:"y"`
}

func BenchmarkCompareHostFunctionStructKwargs(b *testing.B) {
	measure := MustFunction("measure", func(value benchmarkVector) (int, error) {
		return value.X*value.X + value.Y*value.Y, nil
	})
	program := benchmarkCompile(
		b,
		`measure(x=x, y=y)`,
		WithInputs("x", "y"),
		WithFunctions(measure),
	)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		result, err := RunAs[int](benchmarkCtx, program, benchmarkArithmeticInputs)
		if err != nil {
			b.Fatal(err)
		}
		if result != 25 {
			b.Fatal(result)
		}
	}
}

// BenchmarkCompareHostFunctionStrings covers the general (non-int) host
// dispatch, which the redesign moved into the single-hop callback path.
func BenchmarkCompareHostFunctionStrings(b *testing.B) {
	tag := MustFunction("tag", func(s string) string { return "<" + s + ">" })
	program := benchmarkCompile(b, `tag(word)`, WithInputs("word"), WithFunctions(tag))

	inputs := map[string]Value{"word": Str("monty")}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		result, err := RunAs[string](benchmarkCtx, program, inputs)
		if err != nil {
			b.Fatal(err)
		}
		if result != "<monty>" {
			b.Fatal(result)
		}
	}
}

func benchmarkCompile(b *testing.B, code string, opts ...CompileOption) *Program {
	b.Helper()
	program, err := Compile(code, opts...)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(program.Close)
	return program
}

func benchmarkOrderItem(sku, category string, quantity, priceCents int) Value {
	return Dict(
		KV("sku", Str(sku)),
		KV("category", Str(category)),
		KV("quantity", Int(quantity)),
		KV("price_cents", Int(priceCents)),
	)
}

func benchmarkIntList(n int) Value {
	items := make([]Value, n)
	for i := range items {
		items[i] = Int(i + 1)
	}
	return List(items...)
}

func benchmarkExpectDictInt(b *testing.B, value Value, key string, want int) {
	b.Helper()
	got, ok := value.Get(key)
	if !ok || got.Kind() != IntKind {
		b.Fatalf("missing int key %q in %s", key, value.Kind())
	}
	if got.Int() != want {
		b.Fatalf("%s=%d, want %d", key, got.Int(), want)
	}
}
