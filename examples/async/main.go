package main

import (
	"context"
	"fmt"
	"log"

	"github.com/joeychilson/monty"
)

func main() {
	code := `
import asyncio

async def main():
    a, b = await asyncio.gather(foo(), bar())
    return a + b

await main()
`
	program, err := monty.Compile(code)
	if err != nil {
		log.Fatal(err)
	}
	defer program.Close()

	progress, err := program.Start(context.Background(), nil)
	if err != nil {
		panic(err)
	}

	callIDs := map[string]uint32{}
	for {
		switch current := progress.(type) {
		case *monty.NameLookup:
			progress, err = current.Resume(context.Background(), monty.ExternalFunction(current.Name))
		case *monty.FunctionCall:
			callIDs[current.Name] = current.CallID
			progress, err = current.ResumePending(context.Background())
		case *monty.ResolveFutures:
			progress, err = current.Resume(context.Background(),
				monty.FutureValue(callIDs["foo"], monty.Int(10)),
				monty.FutureValue(callIDs["bar"], monty.Int(32)),
			)
		case *monty.Complete:
			fmt.Println("Final value:", current.Value.Int())
			return
		default:
			panic(fmt.Sprintf("unexpected progress %T", progress))
		}
		if err != nil {
			panic(err)
		}
	}
}
