package monty

import "testing"

func TestAsStruct(t *testing.T) {
	type sample struct {
		Count int    `monty:"count"`
		Name  string `monty:"name"`
	}

	value := Dict(
		Pair{Key: Str("count"), Value: Int(2)},
		Pair{Key: Str("name"), Value: Str("ok")},
	)
	got, err := As[sample](value)
	if err != nil {
		t.Fatal(err)
	}
	if got != (sample{Count: 2, Name: "ok"}) {
		t.Fatalf("struct = %+v", got)
	}
}
