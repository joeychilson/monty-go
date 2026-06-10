// Function: host functions with generated type stubs, including concurrent
// asyncio.gather dispatch onto goroutines.
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/joeychilson/monty"
)

type TempArgs struct {
	Lat float64
	Lon float64
}

type Forecast struct {
	City string
	High float64
	Low  float64
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	getForecast := monty.MustFunction("get_forecast",
		func(_ context.Context, _ TempArgs) (Forecast, error) {
			time.Sleep(50 * time.Millisecond) // pretend to call a weather API
			return Forecast{City: "San Francisco", High: 24.5, Low: 18.0}, nil
		},
		monty.WithAsync(),
		monty.WithDoc("Get the weather forecast for a coordinate."),
	)
	fmt.Println("generated stub:")
	fmt.Println(getForecast.PythonStub())
	fmt.Println()

	code := `
import asyncio

async def main():
    west, east = await asyncio.gather(
        get_forecast(lat=37.77, lon=-122.41),
        get_forecast(lat=40.71, lon=-74.00),
    )
    return [west["high"], east["high"]]

await main()
`
	prog, err := monty.Compile(code, monty.WithFunctions(getForecast), monty.WithTypeCheck())
	if err != nil {
		return err
	}
	defer prog.Close()

	started := time.Now()
	highs, err := monty.RunAs[[]float64](context.Background(), prog, nil)
	if err != nil {
		return err
	}
	// Both forecasts ran concurrently in goroutines: ~50ms, not ~100ms.
	fmt.Printf("highs=%v in %v\n", highs, time.Since(started).Round(time.Millisecond))
	return nil
}
