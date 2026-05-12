// Package monty provides Go bindings for Pydantic's Monty sandboxed Python interpreter.
//
// The binding uses purego to load a small Rust cdylib. Go owns the ergonomic API:
// typed inputs, typed results, host function dispatch, and snapshot resume loops.
package monty
