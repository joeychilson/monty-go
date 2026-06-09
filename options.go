package monty

import (
	"io"
	"maps"
	"slices"

	"github.com/joeychilson/monty/internal/ffi"
)

// CompileOption configures Compile.
type CompileOption func(*compileConfig)

type compileConfig struct {
	scriptName string
	inputs     []string
	functions  []*Function
	typeCheck  bool
	typeStubs  string
	autoStubs  bool
}

// WithScriptName sets the filename Monty uses in tracebacks and type-checking diagnostics.
func WithScriptName(name string) CompileOption {
	return func(c *compileConfig) { c.scriptName = name }
}

// WithInputs declares the Python variable names that must be supplied when the program runs.
func WithInputs(names ...string) CompileOption {
	return func(c *compileConfig) { c.inputs = slices.Clone(names) }
}

// WithFunction registers a Go function that Python code may call by name.
func WithFunction(function *Function) CompileOption {
	return func(c *compileConfig) {
		if function != nil {
			c.functions = append(c.functions, function)
		}
	}
}

// WithTypeCheck runs Monty's Python type checker during Compile.
func WithTypeCheck() CompileOption {
	return func(c *compileConfig) { c.typeCheck = true }
}

// WithTypeStubs provides additional Python stub text and enables type checking.
func WithTypeStubs(stubs string) CompileOption {
	return func(c *compileConfig) {
		c.typeStubs = stubs
		c.typeCheck = true
	}
}

// WithAutoStubs enables type checking with stubs generated from registered Go functions.
func WithAutoStubs() CompileOption {
	return func(c *compileConfig) {
		c.autoStubs = true
		c.typeCheck = true
	}
}

// RunOption configures Program.Start, Program.Run, and RunAs.
type RunOption func(*runConfig)

type runConfig struct {
	limits         *Limits
	stdout         io.Writer
	functions      map[string]*Function
	functionNames  []ffi.Str
	functionsOwned bool
	osHandler      OSHandler
	mounts         []Mount
	mountDirs      []*MountDir
	mountHandles   []uintptr
	ownedMounts    []uintptr
}

// WithLimits applies resource limits to a single program run.
func WithLimits(limits Limits) RunOption {
	return func(c *runConfig) { c.limits = new(limits) }
}

// WithStdout sends Python print output to writer.
func WithStdout(writer io.Writer) RunOption {
	return func(c *runConfig) { c.stdout = writer }
}

// WithRunFunction registers a Go function for this run only. A run function
// with the same name as a compile-time function (registered via WithFunction)
// overrides it for that run; this is intentional. Two WithRunFunction options
// with the same name likewise last-write-win.
func WithRunFunction(function *Function) RunOption {
	return func(c *runConfig) {
		if function == nil {
			return
		}
		if c.functions == nil {
			c.functions = map[string]*Function{}
		} else if !c.functionsOwned {
			c.functions = maps.Clone(c.functions)
		}
		c.functionsOwned = true
		c.functions[function.Name()] = function
		c.functionNames = nil
	}
}

// WithOSHandler registers a Go handler for Python OS calls during this run.
func WithOSHandler(handler OSHandler) RunOption {
	return func(c *runConfig) { c.osHandler = handler }
}
