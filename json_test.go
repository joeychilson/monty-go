package monty

import (
	"encoding/json"
	"math"
	"testing"
)

func TestValueNaturalJSON(t *testing.T) {
	value := List(
		None(),
		Tuple(Int(1), Str("x")),
		Bytes([]byte{1, 2}),
		Float(math.Inf(1)),
		BigInt("9223372036854775808"),
		Path("/tmp/data.txt"),
		Exception("ValueError", "bad"),
	)
	got, err := value.JSONString()
	if err != nil {
		t.Fatal(err)
	}
	want := `[null,{"$tuple":[1,"x"]},{"$bytes":[1,2]},{"$float":"inf"},9223372036854775808,{"$path":"/tmp/data.txt"},{"$exception":{"type":"ValueError","arg":"bad"}}]`
	if got != want {
		t.Fatalf("json = %s, want %s", got, want)
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != want {
		t.Fatalf("json.Marshal = %s, want %s", encoded, want)
	}
}
