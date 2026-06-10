package monty

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestValueConstructorsAndAccessors(t *testing.T) {
	cases := []struct {
		value Value
		kind  Kind
		repr  string
	}{
		{None(), NoneKind, "None"},
		{Ellipsis(), EllipsisKind, "Ellipsis"},
		{Bool(true), BoolKind, "True"},
		{Int(42), IntKind, "42"},
		{Int64(-7), IntKind, "-7"},
		{BigInt("123456789012345678901234567890"), BigIntKind, "123456789012345678901234567890"},
		{Float(1.5), FloatKind, "1.5"},
		{Str("hi"), StringKind, "hi"},
		{Bytes([]byte{0x61}), BytesKind, `"a"`},
		{List(Int(1), Int(2)), ListKind, "[1, 2]"},
		{Tuple(Int(1), Str("a")), TupleKind, "(1, a)"},
		{Set(Int(1)), SetKind, "{1}"},
		{FrozenSet(), FrozenSetKind, "frozenset()"},
		{Dict(KV("k", Int(1))), DictKind, "{k: 1}"},
		{Path("/tmp/x").MontyValue(), PathKind, "/tmp/x"},
		{Date{Year: 2026, Month: time.May, Day: 12}.MontyValue(), DateKind, "2026-05-12"},
		{TimeDelta{Days: 1, Seconds: 2, Microseconds: 3}.MontyValue(), TimeDeltaKind, "1d 2s 3us"},
		{Exception{Type: "ValueError", Message: "bad"}.MontyValue(), ExceptionKind, "ValueError: bad"},
	}
	for _, tc := range cases {
		if tc.value.Kind() != tc.kind {
			t.Errorf("%s: kind = %s, want %s", tc.repr, tc.value.Kind(), tc.kind)
		}
		if got := tc.value.String(); got != tc.repr {
			t.Errorf("String() = %q, want %q", got, tc.repr)
		}
	}
}

func TestValueCollections(t *testing.T) {
	dict := Dict(
		KV("a", Int(1)),
		KV("b", Str("two")),
		Pair{Key: Tuple(Int(1), Int(2)), Value: Str("tuple-key")},
	)
	if dict.Len() != 3 {
		t.Fatalf("Len = %d", dict.Len())
	}
	if v, ok := dict.Get("b"); !ok || v.Str() != "two" {
		t.Fatalf("Get(b) = %v %v", v, ok)
	}
	if v, ok := dict.Get([]any{1, 2}); ok {
		t.Fatalf("Get(slice key) should not match a tuple key, got %v", v)
	}
	if _, ok := dict.Get("missing"); ok {
		t.Fatal("Get(missing) reported ok")
	}

	list := List(Int(10), Int(20), Int(30))
	if list.Index(1).Int() != 20 {
		t.Fatalf("Index(1) = %v", list.Index(1))
	}
	if list.Index(5).Kind() != InvalidKind {
		t.Fatal("Index out of range should be invalid")
	}
	var sum int
	for v := range list.Elems() {
		sum += v.Int()
	}
	if sum != 60 {
		t.Fatalf("sum = %d", sum)
	}

	nt := NamedTuple{Type: "Point", Fields: []string{"x", "y"}, Values: []Value{Int(1), Int(2)}}.MontyValue()
	if v, ok := nt.Attr("y"); !ok || v.Int() != 2 {
		t.Fatalf("Attr(y) = %v %v", v, ok)
	}
	if nt.String() != "Point(x=1, y=2)" {
		t.Fatalf("namedtuple repr = %q", nt.String())
	}
}

func TestValueInterface(t *testing.T) {
	value := Dict(
		KV("n", Int(1)),
		KV("items", List(Str("a"), Str("b"))),
	)
	native, ok := value.Interface().(map[any]any)
	if !ok {
		t.Fatalf("Interface() = %T", value.Interface())
	}
	if native["n"] != int64(1) {
		t.Fatalf("n = %v", native["n"])
	}
	items, ok := native["items"].([]any)
	if !ok || len(items) != 2 || items[0] != "a" {
		t.Fatalf("items = %v", native["items"])
	}
}

func TestValueMarshalJSON(t *testing.T) {
	payload := Dict(
		KV("name", Str("x")),
		KV("tuple", Tuple(Int(1), Int(2))),
	)
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["name"] != "x" {
		t.Fatalf("name = %v", decoded["name"])
	}
	tagged, ok := decoded["tuple"].(map[string]any)
	if !ok || tagged["$tuple"] == nil {
		t.Fatalf("tuple = %v", decoded["tuple"])
	}
}

func TestDateTimeRoundTrip(t *testing.T) {
	aware := DateTime{
		Year: 2026, Month: time.June, Day: 9,
		Hour: 12, Minute: 30, Second: 15, Microsecond: 250,
		TZ: &TimeZone{Offset: -7 * time.Hour, Name: "PDT"},
	}
	value, err := Eval(context.Background(), "dt", map[string]any{"dt": aware})
	if err != nil {
		t.Fatal(err)
	}
	got := value.DateTime()
	if got.TZ == nil || got.TZ.Offset != -7*time.Hour || got.TZ.Name != "PDT" {
		t.Fatalf("TZ = %+v", got.TZ)
	}
	if got.Hour != 12 || got.Microsecond != 250 {
		t.Fatalf("got %+v", got)
	}

	naive := DateTime{Year: 2026, Month: time.June, Day: 9}
	value, err = Eval(context.Background(), "dt", map[string]any{"dt": naive})
	if err != nil {
		t.Fatal(err)
	}
	if value.DateTime().TZ != nil {
		t.Fatalf("naive datetime came back aware: %+v", value.DateTime())
	}
}

// TestFlatDecodeLargeResultStrings exercises the flat-decode copy path: a
// result larger than flatStringCopyThreshold must decode strings correctly
// whether they are copied (large buffer) or borrowed (small buffer).
func TestFlatDecodeLargeResultStrings(t *testing.T) {
	code := `[f"item-{i:060d}" for i in range(200)]`
	value, err := Eval(context.Background(), code, nil)
	if err != nil {
		t.Fatal(err)
	}
	if value.Len() != 200 {
		t.Fatalf("len = %d, want 200", value.Len())
	}
	want := fmt.Sprintf("item-%060d", 7)
	if got := value.Index(7).Str(); got != want {
		t.Fatalf("items[7] = %q, want %q", got, want)
	}

	small, err := Eval(context.Background(), `"hello"`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := small.Str(); got != "hello" {
		t.Fatalf("small result = %q, want hello", got)
	}
}

// TestNestedRawDecodeRoundTrip exercises the recursive raw-value decode paths
// on a deeply nested, mixed-container result: every container must be freed
// exactly once on the success path.
func TestNestedRawDecodeRoundTrip(t *testing.T) {
	code := `[
    {"name": "a", "tags": ["x", "y"], "meta": {"n": 1, "vals": [1, 2, 3]}},
    {"name": "b", "tags": [], "meta": {"n": 2, "vals": [(4, 5), (6, 7)]}},
]`
	prog, err := Compile(code)
	if err != nil {
		t.Fatal(err)
	}
	defer prog.Close()
	// Start drives the raw snapshot decode path (vs the flat fast format).
	run, err := prog.Start(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	value, err := run.Result()
	if err != nil {
		t.Fatal(err)
	}
	if value.Kind() != ListKind || value.Len() != 2 {
		t.Fatalf("top-level = kind %s len %d", value.Kind(), value.Len())
	}
	first, err := As[map[string]any](value.Index(0))
	if err != nil {
		t.Fatal(err)
	}
	if first["name"] != "a" {
		t.Fatalf("items[0][name] = %v", first["name"])
	}
	meta, ok := value.Index(1).Get("meta")
	if !ok {
		t.Fatal("missing meta")
	}
	vals, ok := meta.Get("vals")
	if !ok || vals.Len() != 2 {
		t.Fatalf("vals = %v", vals)
	}
	tuple := vals.Index(0)
	if tuple.Kind() != TupleKind || tuple.Index(0).Int() != 4 {
		t.Fatalf("nested tuple = %s", tuple)
	}
}

func TestValueEqualSemantics(t *testing.T) {
	dict := Dict(
		Pair{Key: Int(1), Value: Str("int")},
		Pair{Key: Tuple(Int(1), Int(2)), Value: Str("tuple")},
	)
	if v, ok := dict.Get(1); !ok || v.Str() != "int" {
		t.Fatalf("Get(1) = %v %v", v, ok)
	}
	if v, ok := dict.Get(Tuple(Int(1), Int(2))); !ok || v.Str() != "tuple" {
		t.Fatalf("Get(tuple) = %v %v", v, ok)
	}
	if v, ok := dict.Get(1.0); !ok || v.Str() != "int" {
		t.Fatalf("Get(1.0) should match int key like Python, got %v %v", v, ok)
	}
}

func TestKindStrings(t *testing.T) {
	kinds := []Kind{InvalidKind, NoneKind, IntKind, DictKind, DataclassKind, CycleKind}
	for _, kind := range kinds {
		if kind.String() == "" || slices.Contains([]string{"Kind(0)"}, kind.String()) {
			t.Errorf("Kind %d has bad String %q", kind, kind.String())
		}
	}
}
