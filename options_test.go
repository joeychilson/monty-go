package monty

import (
	"bytes"
	"testing"
	"time"
)

func TestWithLimitsCopiesLimitValue(t *testing.T) {
	limits := Limits{MaxDuration: time.Second}
	var config runConfig
	WithLimits(limits)(&config)
	limits.MaxDuration = 2 * time.Second

	if config.limits == nil {
		t.Fatal("limits not set")
	}
	if config.limits.MaxDuration != time.Second {
		t.Fatalf("MaxDuration = %s, want 1s", config.limits.MaxDuration)
	}
}

func TestRunOptions(t *testing.T) {
	function := NewFunction("identity", func(_ int) (int, error) { return 0, nil })
	var stdout bytes.Buffer
	var config runConfig

	WithStdout(&stdout)(&config)
	WithRunFunction(nil)(&config)
	WithRunFunction(function)(&config)

	if config.stdout != &stdout {
		t.Fatal("stdout writer not set")
	}
	if config.functions[function.Name()] != function {
		t.Fatal("run function not registered")
	}
}

func TestCompileOptions(t *testing.T) {
	function := NewFunction("identity", func(_ int) (int, error) { return 0, nil })
	var config compileConfig

	WithScriptName("worker.py")(&config)
	WithInputs("x", "y")(&config)
	WithFunction(nil)(&config)
	WithFunction(function)(&config)
	WithTypeStubs("def identity(x: int) -> int: ...")(&config)
	WithAutoStubs()(&config)

	if config.scriptName != "worker.py" {
		t.Fatalf("scriptName = %q", config.scriptName)
	}
	if got := len(config.inputs); got != 2 {
		t.Fatalf("inputs length = %d, want 2", got)
	}
	if config.functions[0] != function {
		t.Fatal("compile function not registered")
	}
	if !config.typeCheck || !config.autoStubs {
		t.Fatal("type checking flags not set")
	}
}
