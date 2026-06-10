// Package monty embeds Monty, Pydantic's sandboxed snapshotable Python
// interpreter, in Go programs without cgo.
//
// Compile Python source once with [Compile] and run it many times with
// [Program.Run], or use [Eval] for one-shot execution. Python code calls back
// into Go through registered [Function] values, reads files through mounts or
// any [io/fs.FS], and is bounded by [Limits] and context cancellation.
//
// For agent-style workloads, [Program.Start] returns a [Run]: a pausable,
// snapshotable execution that surfaces every external interaction — function
// calls, OS calls, name lookups, async future batches — as a typed
// [Interrupt] the host answers explicitly. A paused Run serializes with
// [Run.Dump] and resumes later (or elsewhere) with [LoadRun].
//
// [REPL] sessions keep Python state alive across snippets, and the whole
// session can be serialized and restored.
package monty
