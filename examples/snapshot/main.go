package main

import (
	"context"
	"fmt"
	"log"

	"github.com/joeychilson/monty"
)

func main() {
	program, err := monty.Compile(`call_llm(prompt) + " (resumed)"`, monty.WithInputs("prompt"))
	if err != nil {
		log.Fatal(err)
	}
	defer program.Close()

	progress, err := program.Start(context.Background(), monty.Inputs{"prompt": monty.Str("hello")})
	if err != nil {
		panic(err)
	}

	functionCall, ok := progress.(*monty.FunctionCall)
	if !ok {
		panic(fmt.Sprintf("expected FunctionCall pause, got %T", progress))
	}
	fmt.Printf("Paused at %s(%v)\n", functionCall.Name, functionCall.Args)

	snapshot, err := functionCall.Dump()
	if err != nil {
		panic(err)
	}
	if err := functionCall.Close(); err != nil {
		panic(err)
	}

	loaded, err := monty.LoadProgress(snapshot)
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := loaded.Close(); err != nil {
			panic(err)
		}
	}()

	loadedCall, ok := loaded.(*monty.FunctionCall)
	if !ok {
		panic(fmt.Sprintf("loaded progress is %T", loaded))
	}

	final, err := loadedCall.Resume(context.Background(), monty.Str("the answer is 42"))
	if err != nil {
		panic(err)
	}
	complete, ok := final.(*monty.Complete)
	if !ok {
		panic(fmt.Sprintf("expected Complete, got %T", final))
	}
	fmt.Println("Final value:", complete.Value.Str())
}
