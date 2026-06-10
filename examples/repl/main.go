// REPL: a stateful session with embedded files served from an fs.FS and
// type-checked snippets.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"testing/fstest"

	"github.com/joeychilson/monty"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()

	// Any fs.FS works here: embed.FS, os.DirFS, fstest.MapFS, ...
	dataFS := fstest.MapFS{
		"config.json": {Data: []byte(`{"retries": 3}`)},
	}

	repl, err := monty.NewREPL(monty.WithTypeCheck(), monty.WithScriptName("session.py"))
	if err != nil {
		return err
	}
	defer repl.Close()

	_, err = repl.Eval(ctx, `
import json
from pathlib import Path
config = json.loads(Path("/data/config.json").read_text())
retries: int = config["retries"]
`, nil, monty.WithFS("/data", dataFS))
	if err != nil {
		return err
	}

	// Session state persists between snippets.
	retries, err := repl.Eval(ctx, `retries`, nil)
	if err != nil {
		return err
	}
	fmt.Println("retries:", retries.Int())

	// Type errors are caught before the snippet runs.
	_, err = repl.Eval(ctx, `retries + "one"`, nil)
	var typeErr *monty.TypeCheckError
	if errors.As(err, &typeErr) {
		fmt.Println("type checker said:")
		fmt.Println(typeErr.Render(monty.DiagnosticConcise, false))
		return nil
	}
	return err
}
