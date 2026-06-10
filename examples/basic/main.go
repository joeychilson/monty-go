// Basic: compile once, run with typed inputs and a typed result.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/joeychilson/monty"
)

type Coords struct {
	X int
	Y int
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	prog, err := monty.Compile("x * x + y * y", monty.WithInputs("x", "y"))
	if err != nil {
		return err
	}
	defer prog.Close()

	result, err := monty.RunAs[int](context.Background(), prog, Coords{X: 3, Y: 4})
	if err != nil {
		return err
	}
	fmt.Println("3² + 4² =", result) // 25

	// One-shot evaluation without keeping a program around.
	greeting, err := monty.EvalAs[string](
		context.Background(),
		`"hello " + name`,
		map[string]any{"name": "monty"},
	)
	if err != nil {
		return err
	}
	fmt.Println(greeting)
	return nil
}
