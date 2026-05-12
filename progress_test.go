package monty

import (
	"context"
	"strings"
	"testing"
)

func TestSnapshotDumpLoadResume(t *testing.T) {
	program, err := Compile(`call_llm(prompt) + " (resumed)"`, WithInputs("prompt"))
	if err != nil {
		t.Fatal(err)
	}
	defer program.Close()

	progress, err := program.Start(context.Background(), Inputs{"prompt": Str("hello")})
	if err != nil {
		t.Fatal(err)
	}

	functionCall, ok := progress.(*FunctionCall)
	if !ok {
		t.Fatalf("progress = %T, want *FunctionCall", progress)
	}
	if functionCall.Name != "call_llm" {
		t.Fatalf("function = %q, want call_llm", functionCall.Name)
	}
	snapshot, err := functionCall.Dump()
	if err != nil {
		t.Fatal(err)
	}
	if err := functionCall.Close(); err != nil {
		t.Fatal(err)
	}
	if len(snapshot) == 0 {
		t.Fatal("empty snapshot")
	}

	loaded, err := LoadProgress(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := loaded.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	loadedCall, ok := loaded.(*FunctionCall)
	if !ok {
		t.Fatalf("loaded = %T, want *FunctionCall", loaded)
	}
	final, err := loadedCall.Resume(context.Background(), Str("the answer is 42"))
	if err != nil {
		t.Fatal(err)
	}
	complete, ok := final.(*Complete)
	if !ok {
		t.Fatalf("final = %T, want *Complete", final)
	}
	if got := complete.Value.Str(); got != "the answer is 42 (resumed)" {
		t.Fatalf("final value = %q", got)
	}
}

func TestAsyncFutureResume(t *testing.T) {
	code := `
import asyncio

async def main():
    a, b = await asyncio.gather(foo(), bar())
    return a + b

await main()
`
	program, err := Compile(code)
	if err != nil {
		t.Fatal(err)
	}
	defer program.Close()

	progress, err := program.Start(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	callIDs := map[string]uint32{}
	for {
		switch current := progress.(type) {
		case *NameLookup:
			switch current.Name {
			case "foo", "bar":
				progress, err = current.Resume(context.Background(), ExternalFunction(current.Name))
			default:
				progress, err = current.ResumeUndefined(context.Background())
			}
			if err != nil {
				t.Fatal(err)
			}
		case *FunctionCall:
			callIDs[current.Name] = current.CallID
			progress, err = current.ResumePending(context.Background())
			if err != nil {
				t.Fatal(err)
			}
		case *ResolveFutures:
			if got := current.PendingCallIDs(); len(got) != 2 {
				t.Fatalf("pending call ids = %v, want 2 ids", got)
			}
			progress, err = current.Resume(context.Background(),
				FutureValue(callIDs["foo"], Int(10)),
				FutureValue(callIDs["bar"], Int(32)),
			)
			if err != nil {
				t.Fatal(err)
			}
		case *Complete:
			if got := current.Value.Int(); got != 42 {
				t.Fatalf("final value = %d, want 42", got)
			}
			return
		default:
			t.Fatalf("unexpected progress %T", progress)
		}
	}
}

func TestOSCallResumeException(t *testing.T) {
	code := `
from pathlib import Path
Path("/sandbox/missing.txt").read_text()
`
	program, err := Compile(code)
	if err != nil {
		t.Fatal(err)
	}
	defer program.Close()

	progress, err := program.Start(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	osCall, ok := progress.(*OSCall)
	if !ok {
		t.Fatalf("progress = %T, want *OSCall", progress)
	}
	if osCall.Function != "Path.read_text" {
		t.Fatalf("OS function = %q, want Path.read_text", osCall.Function)
	}
	_, err = osCall.ResumeException(context.Background(), "FileNotFoundError", "missing")
	if err == nil {
		t.Fatal("expected FileNotFoundError")
	}
	if !strings.Contains(err.Error(), "FileNotFoundError") {
		t.Fatalf("error = %q, want FileNotFoundError", err)
	}
}
