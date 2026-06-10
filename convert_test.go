package monty

import (
	"context"
	"math/big"
	"testing"
	"time"
)

func TestFromBasicTypes(t *testing.T) {
	cases := []struct {
		input any
		kind  Kind
	}{
		{nil, NoneKind},
		{true, BoolKind},
		{42, IntKind},
		{int8(1), IntKind},
		{uint64(1 << 63), BigIntKind},
		{1.5, FloatKind},
		{"s", StringKind},
		{[]byte{1}, BytesKind},
		{[]int{1, 2}, ListKind},
		{map[string]int{"a": 1}, DictKind},
		{time.Now(), DateTimeKind},
		{time.Second, TimeDeltaKind},
		{big.NewInt(7), IntKind},
		{new(big.Int).Lsh(big.NewInt(1), 100), BigIntKind},
		{Path("/x"), PathKind},
		{Date{Year: 2026, Month: 1, Day: 1}, DateKind},
	}
	for _, tc := range cases {
		value, err := From(tc.input)
		if err != nil {
			t.Errorf("From(%v): %v", tc.input, err)
			continue
		}
		if value.Kind() != tc.kind {
			t.Errorf("From(%v) kind = %s, want %s", tc.input, value.Kind(), tc.kind)
		}
	}
}

func TestFromStructTags(t *testing.T) {
	type inner struct {
		UserID      int    `monty:"uid"`
		Skipped     string `monty:"-"`
		HTTPTimeout int
	}
	value, err := From(inner{UserID: 7, Skipped: "no", HTTPTimeout: 1})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := value.Get("uid"); !ok || v.Int() != 7 {
		t.Fatalf("uid = %v %v", v, ok)
	}
	if _, ok := value.Get("skipped"); ok {
		t.Fatal("skipped field was converted")
	}
	if _, ok := value.Get("http_timeout"); !ok {
		t.Fatal("HTTPTimeout should snake_case to http_timeout")
	}
}

func TestAsAndDecode(t *testing.T) {
	value, err := Eval(context.Background(), `{"name": "ada", "score": 99, "tags": ["a", "b"]}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	type record struct {
		Name  string
		Score int
		Tags  []string
	}
	got, err := As[record](value)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "ada" || got.Score != 99 || len(got.Tags) != 2 {
		t.Fatalf("got %+v", got)
	}

	var decoded record
	if err := value.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Name != "ada" || decoded.Score != 99 {
		t.Fatalf("decoded %+v", decoded)
	}
	if err := value.Decode(record{}); err == nil {
		t.Fatal("Decode of non-pointer should error")
	}
}

func TestAsPayloadTypes(t *testing.T) {
	value, err := Eval(context.Background(), `
from datetime import date, datetime, timedelta, timezone
{"d": date(2026, 5, 12), "dt": datetime(2026, 5, 12, 9, 30), "td": timedelta(seconds=90)}
`, nil)
	if err != nil {
		t.Fatal(err)
	}
	type payload struct {
		D  Date
		DT time.Time `monty:"dt"`
		TD time.Duration
	}
	got, err := As[payload](value)
	if err != nil {
		t.Fatal(err)
	}
	if got.D.Year != 2026 || got.D.Month != time.May {
		t.Fatalf("date = %+v", got.D)
	}
	if got.DT.Hour() != 9 || got.DT.Minute() != 30 {
		t.Fatalf("datetime = %v", got.DT)
	}
	if got.TD != 90*time.Second {
		t.Fatalf("timedelta = %v", got.TD)
	}
}

func TestAsBigInt(t *testing.T) {
	value, err := Eval(context.Background(), "2 ** 100", nil)
	if err != nil {
		t.Fatal(err)
	}
	if value.Kind() != BigIntKind {
		t.Fatalf("kind = %s", value.Kind())
	}
	got, err := As[*big.Int](value)
	if err != nil {
		t.Fatal(err)
	}
	want := new(big.Int).Lsh(big.NewInt(1), 100)
	if got.Cmp(want) != 0 {
		t.Fatalf("got %s", got)
	}
}

func TestAsErrors(t *testing.T) {
	if _, err := As[int](Str("nope")); err == nil {
		t.Fatal("As[int] of a string should error")
	}
	if _, err := As[string](Int(1)); err == nil {
		t.Fatal("As[string] of an int should error")
	}
	if _, err := As[int8](Int(1000)); err == nil {
		t.Fatal("As[int8] overflow should error")
	}
	if _, err := As[uint](Int(-1)); err == nil {
		t.Fatal("As[uint] of a negative should error")
	}
}

func TestInputsForms(t *testing.T) {
	prog, err := Compile("a + b", WithInputs("a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	defer prog.Close()

	ctx := context.Background()
	cases := []any{
		map[string]Value{"a": Int(1), "b": Int(2)},
		map[string]any{"a": 1, "b": 2},
		struct{ A, B int }{1, 2},
		&struct{ A, B int }{1, 2},
	}
	for i, inputs := range cases {
		got, err := RunAs[int](ctx, prog, inputs)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if got != 3 {
			t.Fatalf("case %d: got %d", i, got)
		}
	}

	if _, err := prog.Run(ctx, map[string]Value{"a": Int(1)}); err == nil {
		t.Fatal("missing input should error")
	}
	if _, err := prog.Run(ctx, 42); err == nil {
		t.Fatal("non-struct inputs should error")
	}
}

func TestMustFromPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MustFrom should panic on unsupported types")
		}
	}()
	MustFrom(make(chan int))
}
