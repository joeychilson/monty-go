// Snapshot: pause execution at an external call, serialize it, and resume
// from the snapshot — the core pattern for durable agent workflows.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/joeychilson/monty"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()
	prog, err := monty.Compile(`"The answer is " + call_llm(prompt)`, monty.WithInputs("prompt"))
	if err != nil {
		return err
	}
	defer prog.Close()

	paused, err := prog.Start(ctx, map[string]any{"prompt": "6 * 7?"})
	if err != nil {
		return err
	}

	call, ok := paused.Pending().(*monty.Call)
	if !ok {
		return fmt.Errorf("unexpected interrupt %T", paused.Pending())
	}
	fmt.Printf("paused at %s(%s)\n", call.Name, call.Args[0])

	// Persist the paused execution; this could go to a database and be
	// resumed by another process.
	snapshot, err := paused.Dump()
	if err != nil {
		return err
	}
	paused.Close()
	fmt.Printf("snapshot: %d bytes\n", len(snapshot))

	// ... later, possibly elsewhere ...
	restored, err := monty.LoadRun(snapshot)
	if err != nil {
		return err
	}
	defer restored.Close()

	call, ok = restored.Pending().(*monty.Call)
	if !ok {
		return fmt.Errorf("unexpected interrupt %T", restored.Pending())
	}
	if err := call.Return(ctx, "42"); err != nil {
		return err
	}
	result, err := monty.ResultAs[string](restored)
	if err != nil {
		return err
	}
	fmt.Println(result)
	return nil
}
