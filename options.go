package monty

import (
	"io"
	"io/fs"
	"maps"
	"slices"
)

// CompileOption configures Compile.
type CompileOption interface{ applyCompile(*compileConfig) }

// RunOption configures Program.Run, Program.Start, Eval, REPL.Eval, and
// REPL.Start.
type RunOption interface{ applyRun(*runConfig) }

// REPLOption configures NewREPL session defaults.
type REPLOption interface{ applyREPL(*replConfig) }

// CompileREPLOption applies to both Compile and NewREPL.
type CompileREPLOption interface {
	CompileOption
	REPLOption
}

// RunREPLOption applies to runs and NewREPL session defaults.
type RunREPLOption interface {
	RunOption
	REPLOption
}

// Option applies everywhere: Compile, runs, and NewREPL.
type Option interface {
	CompileOption
	RunOption
	REPLOption
}

// option implements every option interface; constructors return it as a
// narrowed interface so misplacement is a compile-time error.
type option struct {
	compile func(*compileConfig)
	run     func(*runConfig)
	repl    func(*replConfig)
}

func (o option) applyCompile(c *compileConfig) {
	if o.compile != nil {
		o.compile(c)
	}
}

func (o option) applyRun(c *runConfig) {
	if o.run != nil {
		o.run(c)
	}
}

func (o option) applyREPL(c *replConfig) {
	if o.repl != nil {
		o.repl(c)
	}
}

type compileConfig struct {
	scriptName  string
	inputs      []string
	functions   []*Function
	typeCheck   bool
	stubs       string
	dataclasses []*DataclassType
}

type runConfig struct {
	limits         *Limits
	stdout         io.Writer
	stderr         io.Writer
	functions      map[string]*Function
	functionsOwned bool
	osHandler      OSHandler
	mounts         []*MountDir
	fsMounts       []fsMount
	skipTypeCheck  bool
	// deadlineBound records that the context deadline tightened MaxDuration,
	// so a TimeoutError can be attributed to the context.
	deadlineBound bool
}

type replConfig struct {
	scriptName  string
	limits      *Limits
	stdout      io.Writer
	stderr      io.Writer
	functions   []*Function
	typeCheck   bool
	stubs       string
	dataclasses []*DataclassType
}

// WithScriptName sets the filename used in tracebacks and type-checking
// diagnostics.
func WithScriptName(name string) CompileREPLOption {
	return option{
		compile: func(c *compileConfig) { c.scriptName = name },
		repl:    func(c *replConfig) { c.scriptName = name },
	}
}

// WithInputs declares the Python variable names that must be supplied when
// the program runs.
func WithInputs(names ...string) CompileOption {
	return option{compile: func(c *compileConfig) { c.inputs = slices.Clone(names) }}
}

// WithFunctions registers Go host functions that Python code may call by
// name. On Compile and NewREPL the functions also feed type-check stub
// generation; on a run they add to (or override, by name) the compile-time
// set for that run only.
func WithFunctions(functions ...*Function) Option {
	return option{
		compile: func(c *compileConfig) {
			for _, function := range functions {
				if function != nil {
					c.functions = append(c.functions, function)
				}
			}
		},
		run: func(c *runConfig) {
			for _, function := range functions {
				if function == nil {
					continue
				}
				if c.functions == nil {
					c.functions = map[string]*Function{}
				} else if !c.functionsOwned {
					c.functions = maps.Clone(c.functions)
				}
				c.functionsOwned = true
				c.functions[function.Name()] = function
			}
		},
		repl: func(c *replConfig) {
			for _, function := range functions {
				if function != nil {
					c.functions = append(c.functions, function)
				}
			}
		},
	}
}

// WithTypeCheck enables Monty's static type checker: Compile checks the
// program (with stubs generated from registered functions and dataclasses),
// and a REPL session checks each snippet against the accumulated session
// context.
func WithTypeCheck() CompileREPLOption {
	return option{
		compile: func(c *compileConfig) { c.typeCheck = true },
		repl:    func(c *replConfig) { c.typeCheck = true },
	}
}

// WithStubs provides extra Python stub text for type checking (input variable
// declarations, external signatures). It implies WithTypeCheck.
func WithStubs(stubs string) CompileREPLOption {
	return option{
		compile: func(c *compileConfig) {
			c.stubs = stubs
			c.typeCheck = true
		},
		repl: func(c *replConfig) {
			c.stubs = stubs
			c.typeCheck = true
		},
	}
}

// WithoutTypeCheck skips the session type check for one REPL snippet and
// keeps that snippet out of the accumulated type-check context. It applies
// only to REPL.Eval / REPL.Start; Program runs reject it.
func WithoutTypeCheck() RunOption {
	return option{run: func(c *runConfig) { c.skipTypeCheck = true }}
}

// WithDataclasses registers dataclass bindings created by DataclassFor:
// their stubs join type checking and inputs of the bound Go types encode as
// dataclass instances.
func WithDataclasses(types ...*DataclassType) CompileREPLOption {
	return option{
		compile: func(c *compileConfig) {
			for _, t := range types {
				if t != nil {
					c.dataclasses = append(c.dataclasses, t)
				}
			}
		},
		repl: func(c *replConfig) {
			for _, t := range types {
				if t != nil {
					c.dataclasses = append(c.dataclasses, t)
				}
			}
		},
	}
}

// WithLimits applies resource limits to a run, or sets the session limits of
// a REPL (which must be created with limits for per-snippet duration budgets
// to apply).
func WithLimits(limits Limits) RunREPLOption {
	return option{
		run:  func(c *runConfig) { c.limits = &limits },
		repl: func(c *replConfig) { c.limits = &limits },
	}
}

// WithStdout streams Python's stdout print output to writer.
func WithStdout(writer io.Writer) RunREPLOption {
	return option{
		run:  func(c *runConfig) { c.stdout = writer },
		repl: func(c *replConfig) { c.stdout = writer },
	}
}

// WithStderr streams Python's stderr print output to writer.
func WithStderr(writer io.Writer) RunREPLOption {
	return option{
		run:  func(c *runConfig) { c.stderr = writer },
		repl: func(c *replConfig) { c.stderr = writer },
	}
}

// WithOSHandler registers a Go handler for Python OS calls (filesystem,
// environment, clock). It runs after mounts and WithFS filesystems decline;
// return ErrNotHandled to fall through to Monty's default behavior.
func WithOSHandler(handler OSHandler) RunOption {
	return option{run: func(c *runConfig) { c.osHandler = handler }}
}

// WithMount attaches a reusable filesystem mount to the run.
func WithMount(mount *MountDir) RunOption {
	return option{run: func(c *runConfig) {
		if mount != nil {
			c.mounts = append(c.mounts, mount)
		}
	}}
}

// WithFS serves read-only Path operations under virtualPath from any fs.FS
// (an embed.FS, os.DirFS, fstest.MapFS, ...). Mounts are consulted first;
// writes are reported to Python as PermissionError.
func WithFS(virtualPath string, fsys fs.FS) RunOption {
	return option{run: func(c *runConfig) {
		if fsys != nil {
			c.fsMounts = append(c.fsMounts, newFSMount(virtualPath, fsys))
		}
	}}
}
