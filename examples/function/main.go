package main

import (
	"context"
	"fmt"
	"log"

	"github.com/joeychilson/monty"
)

type TempArgs struct {
	Lat float64 `monty:"lat"`
	Lng float64 `monty:"lng"`
}

type Forecast struct {
	City string  `monty:"city"`
	High float64 `monty:"high"`
	Low  float64 `monty:"low"`
}

func main() {
	getTemp := monty.NewFunction("get_temperature", func(_ context.Context, _ TempArgs) (Forecast, error) {
		return Forecast{City: "San Francisco", High: 24.5, Low: 18.0}, nil
	}, monty.WithDoc("Get the weather forecast for a coordinate."))

	fmt.Println(getTemp.PythonStub())

	code := `
forecast = get_temperature(lat=lat, lng=lng)
f"{forecast['city']}: high {forecast['high']}C, low {forecast['low']}C"
`
	program, err := monty.Compile(code,
		monty.WithInputs("lat", "lng"),
		monty.WithFunction(getTemp),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer program.Close()

	result, err := monty.RunAs[string](context.Background(), program, monty.Inputs{
		"lat": monty.Float(37.7749),
		"lng": monty.Float(-122.4194),
	})
	if err != nil {
		panic(err)
	}
	fmt.Println("Python output:", result)
}
