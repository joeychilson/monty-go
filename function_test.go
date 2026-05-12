package monty

import (
	"context"
	"strings"
	"testing"
)

type tempArgs struct {
	Lat float64 `monty:"lat"`
	Lng float64 `monty:"lng"`
}

type forecast struct {
	City string  `monty:"city"`
	High float64 `monty:"high"`
	Low  float64 `monty:"low"`
}

func TestFunctionDispatch(t *testing.T) {
	getTemp := NewFunction("get_temperature", func(_ context.Context, _ tempArgs) (forecast, error) {
		return forecast{City: "San Francisco", High: 24.5, Low: 18.0}, nil
	}, WithDoc("Get the weather forecast for a coordinate."))

	if stub := getTemp.PythonStub(); !strings.Contains(stub, "def get_temperature") {
		t.Fatalf("stub missing function: %s", stub)
	}

	code := `
forecast = get_temperature(lat=lat, lng=lng)
f"{forecast['city']}: high {forecast['high']}C, low {forecast['low']}C"
`
	program, err := Compile(code, WithInputs("lat", "lng"), WithFunction(getTemp))
	if err != nil {
		t.Fatal(err)
	}
	defer program.Close()

	result, err := RunAs[string](context.Background(), program, Inputs{
		"lat": Float(37.7749),
		"lng": Float(-122.4194),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "San Francisco: high 24.5C, low 18.0C"
	if result != want {
		t.Fatalf("result = %q, want %q", result, want)
	}
}
