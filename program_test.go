package monty

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type coords struct {
	X int `monty:"x"`
	Y int `monty:"y"`
}

func TestRunAsInt(t *testing.T) {
	program, err := Compile("x * x + y * y", WithInputs("x", "y"))
	if err != nil {
		t.Fatal(err)
	}
	defer program.Close()

	result, err := RunAs[int](context.Background(), program, InputsOf(coords{X: 3, Y: 4}))
	if err != nil {
		t.Fatal(err)
	}
	if result != 25 {
		t.Fatalf("result = %d, want 25", result)
	}
}

func TestCompileCloseCompile(t *testing.T) {
	first, err := Compile("1")
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	second, err := Compile("2")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.handle == 0 {
		t.Fatal("second handle is zero")
	}
}

func TestRunCloseCompile(t *testing.T) {
	first, err := Compile("x + 1", WithInputs("x"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = RunAs[int](context.Background(), first, Inputs{"x": Int(1)})
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	second, err := Compile("2")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.handle == 0 {
		t.Fatal("second handle is zero")
	}
}

func TestProgramDumpLoadPreservesMetadata(t *testing.T) {
	program, err := Compile("x + y", WithScriptName("calc.py"), WithInputs("x", "y"))
	if err != nil {
		t.Fatal(err)
	}
	defer program.Close()

	snapshot, err := program.Dump()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadProgram(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Close()

	if loaded.Code() != "x + y" {
		t.Fatalf("code = %q, want source", loaded.Code())
	}
	if loaded.ScriptName() != "calc.py" {
		t.Fatalf("script name = %q, want calc.py", loaded.ScriptName())
	}
	if got := strings.Join(loaded.Inputs(), ","); got != "x,y" {
		t.Fatalf("inputs = %q, want x,y", got)
	}
	result, err := RunAs[int](context.Background(), loaded, Inputs{"x": Int(2), "y": Int(3)})
	if err != nil {
		t.Fatal(err)
	}
	if result != 5 {
		t.Fatalf("result = %d, want 5", result)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, fmt.Errorf("writer failed")
}

func TestStdoutWriterError(t *testing.T) {
	program, err := Compile("print('hello')\n1")
	if err != nil {
		t.Fatal(err)
	}
	defer program.Close()

	_, err = program.Run(context.Background(), nil, WithStdout(failingWriter{}))
	if err == nil {
		t.Fatal("expected stdout writer error")
	}
	if !strings.Contains(err.Error(), "writer failed") {
		t.Fatalf("error = %q, want writer failure", err)
	}
}

func TestCompileAndRunWithRunFunction(t *testing.T) {
	double := NewFunction("double", func(value int) (int, error) {
		return value * 2, nil
	})

	result, err := CompileAndRunAs[int](
		context.Background(),
		"double(x) + 1",
		Inputs{"x": Int(20)},
		WithRunFunction(double),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result != 41 {
		t.Fatalf("result = %d, want 41", result)
	}
}

func BenchmarkRunArithmetic(b *testing.B) {
	program, err := Compile("x * x + y * y", WithInputs("x", "y"))
	if err != nil {
		b.Fatal(err)
	}
	defer program.Close()
	inputs := Inputs{"x": Int(3), "y": Int(4)}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		result, err := RunAs[int](ctx, program, inputs)
		if err != nil {
			b.Fatal(err)
		}
		if result != 25 {
			b.Fatal(result)
		}
	}
}

func Example() {
	program, err := Compile("x * x + y * y", WithInputs("x", "y"))
	if err != nil {
		panic(err)
	}
	defer program.Close()

	result, err := RunAs[int](context.Background(), program, InputsOf(coords{X: 3, Y: 4}))
	if err != nil {
		panic(err)
	}
	fmt.Println(result)
	// Output: 25
}
