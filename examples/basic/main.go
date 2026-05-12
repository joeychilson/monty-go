package main

import (
	"context"
	"fmt"
	"log"

	"github.com/joeychilson/monty"
)

type Coords struct {
	X int `monty:"x"`
	Y int `monty:"y"`
}

func main() {
	program, err := monty.Compile("x * x + y * y", monty.WithInputs("x", "y"))
	if err != nil {
		log.Fatal(err)
	}
	defer program.Close()

	result, err := monty.RunAs[int](context.Background(), program, monty.InputsOf(Coords{X: 3, Y: 4}))
	if err != nil {
		panic(err)
	}
	fmt.Printf("3^2 + 4^2 = %d\n", result)
}
