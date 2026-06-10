package ffi

import (
	"runtime"
	"sync"
)

var (
	runJSONOutputPool = sync.Pool{
		New: func() any { return new(RunJSONOutput) },
	}
	// Snapshot output structs must be heap-allocated (hence a pool) rather than a
	// stack-local var: their address is smuggled to the Rust trampoline through a
	// uintptr, which defeats escape analysis. A stack-local could then be moved
	// by a stack growth mid-call — the trampoline does not keep the goroutine in
	// syscall state — leaving Rust writing to a stale address and the Go-visible
	// struct all zeros. Heap allocations do not move, so the write lands.
	progressSnapshotOutputPool = sync.Pool{
		New: func() any { return new(ProgressSnapshotOutput) },
	}
)

// resetFastOutput clears every header field that the Rust side writes back
// without touching the 8 KiB Scratch payload. Zeroing the full struct each
// call would memset all of Scratch even when no flat payload follows.
func resetFastOutput(out *RunFastOutput) {
	out.Format = 0
	out.BytesInScratch = 0
	out.PrintFlags = 0
	out.Value = RawValue{}
	out.Bytes = Bytes{}
	out.Print = Bytes{}
	out.Error = 0
}

// stepFromOutput converts a pooled snapshot output into a StepResult,
// consuming the print buffer and (on failure) the error handle. The REPL
// handle is surfaced in both cases — ownership transfers to the caller even
// when the step failed (preserved session semantics).
func stepFromOutput(out *ProgressSnapshotOutput, status uintptr) (StepResult, error) {
	result := StepResult{
		Progress: out.Progress,
		Repl:     out.Repl,
		Snapshot: out.Snapshot,
		Print:    TakePrinted(out.Print, out.PrintFlags),
	}
	switch status {
	case StatusOK:
		return result, nil
	case StatusErrRetained:
		return result, &RetainedError{Err: TakeError(out.Error)}
	default:
		return result, TakeError(out.Error)
	}
}

// --------------------------------------------------------------------------
// Cancellation tokens
// --------------------------------------------------------------------------

// CancelTokenNew creates a cancellation token usable in Limits.CancelToken or
// ReplFeedArgs.CancelToken. Release with CancelTokenFree once no execution
// references it.
func CancelTokenNew() (uintptr, error) {
	if err := EnsureLoaded(); err != nil {
		return 0, err
	}
	return mgCancelTokenNew(), nil
}

// CancelTokenCancel requests cancellation; safe to call from any goroutine
// while another goroutine is blocked in a run/resume call using the token.
func CancelTokenCancel(token uintptr) {
	if token != 0 {
		mgCancelTokenCancel(token)
	}
}

// CancelTokenFree releases a cancellation token.
func CancelTokenFree(token uintptr) {
	if token != 0 {
		mgCancelTokenFree(token)
	}
}

// --------------------------------------------------------------------------
// Type checking
// --------------------------------------------------------------------------

// TypeCheck runs Rust-side type checking for code and optional stubs. On
// success it returns 0 (clean) or a diagnostics handle that must be released
// with DiagnosticsFree.
func TypeCheck(code, scriptName, stubs, stubsName string) (uintptr, error) {
	if err := EnsureLoaded(); err != nil {
		return 0, err
	}
	args := TypeCheckArgs{
		Code:       StringRef(code),
		ScriptName: StringRef(scriptName),
		Stubs:      StringRef(stubs),
		StubsName:  StringRef(stubsName),
	}
	var diags, errHandle uintptr
	status := mgTypeCheck(ptrOf(&args), ptrOf(&diags), ptrOf(&errHandle))
	runtime.KeepAlive(&args)
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return diags, nil
}

// DiagnosticsRender renders a diagnostics handle in one of the upstream
// formats ("full", "concise", "azure", "json", "jsonlines", "rdjson",
// "pylint", "gitlab", "github").
func DiagnosticsRender(diags uintptr, format string, color bool) (string, error) {
	var out Bytes
	var errHandle uintptr
	formatPtr, formatLen := stringArgs(format)
	status := mgDiagnosticsRender(diags, formatPtr, formatLen, boolByte(color), ptrOf(&out), ptrOf(&errHandle))
	if status != StatusOK {
		return "", TakeError(errHandle)
	}
	return TakeString(out), nil
}

// DiagnosticsFree releases a diagnostics handle.
func DiagnosticsFree(diags uintptr) {
	if diags != 0 {
		mgDiagnosticsFree(diags)
	}
}

// --------------------------------------------------------------------------
// Mounts
// --------------------------------------------------------------------------

// MountNew creates a Rust-side mount handle.
func MountNew(virtualPath, hostPath string, mode uint32, writeBytesLimit *uint64) (uintptr, error) {
	if err := EnsureLoaded(); err != nil {
		return 0, err
	}
	args := MountNewArgs{
		VirtualPath: StringRef(virtualPath),
		HostPath:    StringRef(hostPath),
		Mode:        mode,
	}
	if writeBytesLimit != nil {
		args.HasWriteBytesLimit = 1
		args.WriteBytesLimit = *writeBytesLimit
	}
	var handle, errHandle uintptr
	status := mgMountNew(ptrOf(&args), ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// MountFree releases a Rust-side mount handle.
func MountFree(handle uintptr) {
	if handle != 0 {
		mgMountFree(handle)
	}
}

// MountHandleOSCall asks the mounted filesystem layer to handle one OS call.
// The returned RawValue (when handled) is caller-owned: consume it via a
// resume call or release it with RawValueFree.
func MountHandleOSCall(mounts []uintptr, function string, args []RawValue, kwargs []RawPair) (RawValue, bool, error) {
	if err := EnsureLoaded(); err != nil {
		return RawValue{}, false, err
	}
	callArgs := MountCallArgs{
		Mounts:     slicePointer(mounts),
		MountCount: uintptr(len(mounts)),
		Function:   StringRef(function),
		Args:       slicePointer(args),
		ArgCount:   uintptr(len(args)),
		Kwargs:     slicePointer(kwargs),
		KwargCount: uintptr(len(kwargs)),
	}
	var out MountOutput
	status := mgMountHandleOSCall(ptrOf(&callArgs), ptrOf(&out))
	runtime.KeepAlive(mounts)
	runtime.KeepAlive(args)
	runtime.KeepAlive(kwargs)
	if status != StatusOK {
		return RawValue{}, out.Handled != 0, TakeError(out.Error)
	}
	return out.Value, out.Handled != 0, nil
}

// --------------------------------------------------------------------------
// REPL
// --------------------------------------------------------------------------

// ReplNew creates a Rust-side REPL handle.
func ReplNew(scriptName string, limits *Limits) (uintptr, error) {
	if err := EnsureLoaded(); err != nil {
		return 0, err
	}
	args := ReplNewArgs{ScriptName: StringRef(scriptName)}
	if limits != nil {
		args.Limits = ptrOf(limits)
	}
	var handle, errHandle uintptr
	status := mgReplNew(ptrOf(&args), ptrOf(&handle), ptrOf(&errHandle))
	runtime.KeepAlive(limits)
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ReplFree releases a Rust-side REPL handle.
func ReplFree(handle uintptr) {
	if handle != 0 {
		mgReplFree(handle)
	}
}

// ReplDump serializes a REPL handle into a snapshot buffer.
func ReplDump(handle uintptr) ([]byte, error) {
	var buffer Bytes
	var errHandle uintptr
	status := mgReplDump(handle, ptrOf(&buffer), ptrOf(&errHandle))
	if status != StatusOK {
		return nil, TakeError(errHandle)
	}
	return TakeBytes(buffer), nil
}

// ReplLoad restores a REPL handle from a snapshot created by ReplDump.
func ReplLoad(snapshot []byte) (uintptr, error) {
	if err := EnsureLoaded(); err != nil {
		return 0, err
	}
	snapshotPtr, snapshotLen := BytesRef(snapshot)
	var handle, errHandle uintptr
	status := mgReplLoad(snapshotPtr, snapshotLen, ptrOf(&handle), ptrOf(&errHandle))
	runtime.KeepAlive(snapshot)
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ReplFeedRunRaw executes one snippet to completion with host dispatch. The
// session handle remains valid afterwards regardless of outcome. The caller
// owns out and must release any handles or buffers it contains; print output
// arrives via out.Print/out.PrintFlags (or the streaming callback).
func ReplFeedRunRaw(repl uintptr, args *ReplFeedArgs, out *RunFastOutput) error {
	if err := EnsureLoaded(); err != nil {
		return err
	}
	resetFastOutput(out)
	status := syscall3(
		mgReplFeedRunRawAddr,
		repl,
		uintptr(ptrOf(args)),
		uintptr(ptrOf(out)),
	)
	// The trampoline passes these as bare uintptr, so the GC does not see them
	// as live across the Rust call; pin them until it returns.
	runtime.KeepAlive(args)
	runtime.KeepAlive(out)
	if status != StatusOK {
		return TakeError(out.Error)
	}
	return nil
}

// ReplFeedStart begins a suspendable snippet execution, consuming the REPL
// session (it moves into the returned progress and is handed back through
// StepResult.Repl at completion or with a preserved-session error).
func ReplFeedStart(repl uintptr, args *ReplFeedArgs) (StepResult, error) {
	if err := EnsureLoaded(); err != nil {
		return StepResult{}, err
	}
	out := progressSnapshotOutputPool.Get().(*ProgressSnapshotOutput) //nolint:errcheck // pool only stores *ProgressSnapshotOutput
	*out = ProgressSnapshotOutput{}
	defer progressSnapshotOutputPool.Put(out)
	status := syscall3(
		mgReplFeedStartAddr,
		repl,
		uintptr(ptrOf(args)),
		uintptr(ptrOf(out)),
	)
	runtime.KeepAlive(args)
	runtime.KeepAlive(out)
	return stepFromOutput(out, status)
}

// ReplCallRaw calls a named Python function defined in the session. The
// caller owns out and must release any handles or buffers it contains.
func ReplCallRaw(repl uintptr, name string, args []RawValue, out *RunFastOutput) error {
	if err := EnsureLoaded(); err != nil {
		return err
	}
	callArgs := ReplCallArgs{
		Name:     StringRef(name),
		Args:     slicePointer(args),
		ArgCount: uintptr(len(args)),
	}
	resetFastOutput(out)
	status := syscall3(
		mgReplCallRawAddr,
		repl,
		uintptr(ptrOf(&callArgs)),
		uintptr(ptrOf(out)),
	)
	runtime.KeepAlive(&callArgs)
	runtime.KeepAlive(args)
	runtime.KeepAlive(name)
	runtime.KeepAlive(out)
	if status != StatusOK {
		return TakeError(out.Error)
	}
	return nil
}

// ReplFunctionNames returns Python function names defined in the REPL.
func ReplFunctionNames(repl uintptr) ([]string, error) {
	var handle, errHandle uintptr
	status := mgReplFunctionNames(repl, ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return nil, TakeError(errHandle)
	}
	defer ValueFree(handle)
	return stringListFromValue(handle)
}

// ReplHasFunction reports whether the REPL contains a named Python function.
func ReplHasFunction(repl uintptr, name string) bool {
	namePtr, nameLen := stringArgs(name)
	return mgReplHasFunction(repl, namePtr, nameLen) != 0
}

// ReplContinuationMode returns the parser continuation mode for interactive code.
func ReplContinuationMode(code string) (uint32, error) {
	if err := EnsureLoaded(); err != nil {
		return 0, err
	}
	codePtr, codeLen := stringArgs(code)
	return mgReplContinuationMode(codePtr, codeLen), nil
}

// --------------------------------------------------------------------------
// Programs
// --------------------------------------------------------------------------

// ProgramCompile compiles code into a Rust-side program handle.
func ProgramCompile(code, scriptName string, inputNames []string) (uintptr, error) {
	if err := EnsureLoaded(); err != nil {
		return 0, err
	}
	names := StringRefs(inputNames)
	args := ProgramCompileArgs{
		Code:       StringRef(code),
		ScriptName: StringRef(scriptName),
		InputNames: slicePointer(names),
		InputCount: uintptr(len(names)),
	}
	var handle, errHandle uintptr
	status := mgProgramCompile(ptrOf(&args), ptrOf(&handle), ptrOf(&errHandle))
	runtime.KeepAlive(names)
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return handle, nil
}

// ProgramFree releases a Rust-side program handle.
func ProgramFree(handle uintptr) {
	if handle != 0 {
		mgProgramFree(handle)
	}
}

// ProgramStartRawSnapshot starts a program and returns the first progress
// state in a single FFI hop. StepResult.Progress is 0 when the run finished.
func ProgramStartRawSnapshot(program uintptr, inputs []RawValue, limits *Limits) (StepResult, error) {
	if err := EnsureLoaded(); err != nil {
		return StepResult{}, err
	}
	out := progressSnapshotOutputPool.Get().(*ProgressSnapshotOutput) //nolint:errcheck // pool only stores *ProgressSnapshotOutput
	*out = ProgressSnapshotOutput{}
	defer progressSnapshotOutputPool.Put(out)
	status := syscall5(
		mgProgramStartRawSnapshotAddr,
		program,
		sliceAddress(inputs),
		uintptr(len(inputs)),
		uintptr(ptrOf(limits)),
		uintptr(ptrOf(out)),
	)
	runtime.KeepAlive(inputs)
	runtime.KeepAlive(limits)
	runtime.KeepAlive(out)
	return stepFromOutput(out, status)
}

// ProgramRunHostRaw runs a program to completion in one hop, dispatching host
// functions, mounts, and OS calls through the callback in args. The caller
// owns out and must release any handles or buffers it contains.
func ProgramRunHostRaw(program uintptr, args *RunHostArgs, out *RunFastOutput) error {
	if err := EnsureLoaded(); err != nil {
		return err
	}
	resetFastOutput(out)
	status := syscall3(
		mgProgramRunHostRawAddr,
		program,
		uintptr(ptrOf(args)),
		uintptr(ptrOf(out)),
	)
	// Keeping args alive transitively pins everything it references (inputs,
	// host name Strs, mounts, limits).
	runtime.KeepAlive(args)
	runtime.KeepAlive(out)
	if status != StatusOK {
		return TakeError(out.Error)
	}
	return nil
}

// ProgramCompileRunFastRaw compiles a program, runs it once, and frees it in
// a single FFI hop. The caller owns out and is responsible for releasing any
// handles or buffers (Value, Bytes) it contains.
func ProgramCompileRunFastRaw(args *CompileRunFastRawArgs, out *RunFastOutput) error {
	if err := EnsureLoaded(); err != nil {
		return err
	}
	resetFastOutput(out)
	status := syscall2(
		mgProgramCompileRunFastAddr,
		uintptr(ptrOf(args)),
		uintptr(ptrOf(out)),
	)
	// Keeping args alive transitively pins everything it references: the Code
	// string, the InputNames/InputValues backing arrays, and Limits.
	runtime.KeepAlive(args)
	runtime.KeepAlive(out)
	if status != StatusOK {
		return TakeError(out.Error)
	}
	return nil
}

// ProgramRunFastRaw runs a program using Rust's fastest selected raw output
// format. The caller owns out and is responsible for releasing any handles or
// buffers (Value, Bytes) it contains.
func ProgramRunFastRaw(program uintptr, inputs []RawValue, limits *Limits, out *RunFastOutput) error {
	if err := EnsureLoaded(); err != nil {
		return err
	}
	resetFastOutput(out)
	status := syscall5(
		mgProgramRunFastAddr,
		program,
		sliceAddress(inputs),
		uintptr(len(inputs)),
		uintptr(ptrOf(limits)),
		uintptr(ptrOf(out)),
	)
	runtime.KeepAlive(inputs)
	runtime.KeepAlive(limits)
	runtime.KeepAlive(out)
	if status != StatusOK {
		return TakeError(out.Error)
	}
	return nil
}

// ProgramRunJSONRaw runs a program and returns its JSON bytes.
func ProgramRunJSONRaw(program uintptr, inputs []RawValue, limits *Limits) ([]byte, Printed, error) {
	if err := EnsureLoaded(); err != nil {
		return nil, Printed{}, err
	}
	out := runJSONOutputPool.Get().(*RunJSONOutput) //nolint:errcheck // pool only stores *RunJSONOutput
	*out = RunJSONOutput{}
	defer runJSONOutputPool.Put(out)
	status := syscall5(
		mgProgramRunJSONAddr,
		program,
		sliceAddress(inputs),
		uintptr(len(inputs)),
		uintptr(ptrOf(limits)),
		uintptr(ptrOf(out)),
	)
	runtime.KeepAlive(inputs)
	runtime.KeepAlive(limits)
	runtime.KeepAlive(out)
	printed := TakePrinted(out.Print, out.PrintFlags)
	if status != StatusOK {
		return nil, printed, TakeError(out.Error)
	}
	return TakeBytes(out.Value), printed, nil
}

// ProgramDump serializes a program handle.
func ProgramDump(program uintptr) ([]byte, error) {
	var buffer Bytes
	var errHandle uintptr
	status := mgProgramDump(program, ptrOf(&buffer), ptrOf(&errHandle))
	if status != StatusOK {
		return nil, TakeError(errHandle)
	}
	return TakeBytes(buffer), nil
}

// ProgramCode returns the source code stored in a program handle.
func ProgramCode(program uintptr) (string, error) {
	var buffer Bytes
	var errHandle uintptr
	status := mgProgramCode(program, ptrOf(&buffer), ptrOf(&errHandle))
	if status != StatusOK {
		return "", TakeError(errHandle)
	}
	return TakeString(buffer), nil
}

// ProgramScriptName returns the script name stored in a program handle.
func ProgramScriptName(program uintptr) (string, error) {
	var buffer Bytes
	var errHandle uintptr
	status := mgProgramScriptName(program, ptrOf(&buffer), ptrOf(&errHandle))
	if status != StatusOK {
		return "", TakeError(errHandle)
	}
	return TakeString(buffer), nil
}

// ProgramInputNames returns the ordered input names stored in a program handle.
func ProgramInputNames(program uintptr) ([]string, error) {
	var handle, errHandle uintptr
	status := mgProgramInputNames(program, ptrOf(&handle), ptrOf(&errHandle))
	if status != StatusOK {
		return nil, TakeError(errHandle)
	}
	defer ValueFree(handle)
	return stringListFromValue(handle)
}

// ProgramLoad restores a program handle from a snapshot created by ProgramDump.
func ProgramLoad(snapshot []byte) (uintptr, error) {
	if err := EnsureLoaded(); err != nil {
		return 0, err
	}
	snapshotPtr, snapshotLen := BytesRef(snapshot)
	var programHandle, errHandle uintptr
	status := mgProgramLoad(snapshotPtr, snapshotLen, ptrOf(&programHandle), ptrOf(&errHandle))
	runtime.KeepAlive(snapshot)
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return programHandle, nil
}

// --------------------------------------------------------------------------
// Progress
// --------------------------------------------------------------------------

// ProgressFree releases a Rust-side progress handle.
func ProgressFree(handle uintptr) {
	if handle != 0 {
		mgProgressFree(handle)
	}
}

// ProgressSnapshotGet returns a snapshot of a live progress handle without
// consuming it (used after ProgressLoad). The caller owns the snapshot's
// buffers.
func ProgressSnapshotGet(handle uintptr) (ProgressSnapshot, error) {
	var out ProgressSnapshot
	var errHandle uintptr
	status := mgProgressSnapshot(handle, ptrOf(&out), ptrOf(&errHandle))
	if status != StatusOK {
		return ProgressSnapshot{}, TakeError(errHandle)
	}
	return out, nil
}

// ProgressPendingLen returns the number of pending future call IDs.
func ProgressPendingLen(handle uintptr) uintptr { return mgProgressPendingLen(handle) }

// ProgressPendingID returns the pending future call ID at index i.
func ProgressPendingID(handle uintptr, i uintptr) uint32 { return mgProgressPendingID(handle, i) }

// ProgressIsRepl reports whether the progress belongs to a REPL execution
// (its completion hands back a session handle).
func ProgressIsRepl(handle uintptr) bool { return mgProgressIsRepl(handle) != 0 }

// ProgressSetCancelToken attaches a cancellation token to a loaded progress
// handle. Returns false when the paused state does not expose its tracker
// (only tracked function-call states do upstream); cancellation is then
// enforced at resume boundaries only.
func ProgressSetCancelToken(handle, token uintptr) bool {
	return mgProgressSetCancelToken(handle, token) != 0
}

// ProgressCallJSON serializes the pending call's args and kwargs in Monty's
// natural JSON form.
func ProgressCallJSON(handle uintptr) ([]byte, []byte, error) {
	var args, kwargs Bytes
	var errHandle uintptr
	status := mgProgressCallJSON(handle, ptrOf(&args), ptrOf(&kwargs), ptrOf(&errHandle))
	if status != StatusOK {
		return nil, nil, TakeError(errHandle)
	}
	return TakeBytes(args), TakeBytes(kwargs), nil
}

// resumeStep runs one already-bound resume trampoline against a pooled
// snapshot output.
func resumeStep(call func(out *ProgressSnapshotOutput) uintptr) (StepResult, error) {
	out := progressSnapshotOutputPool.Get().(*ProgressSnapshotOutput) //nolint:errcheck // pool only stores *ProgressSnapshotOutput
	*out = ProgressSnapshotOutput{}
	defer progressSnapshotOutputPool.Put(out)
	status := call(out)
	runtime.KeepAlive(out)
	return stepFromOutput(out, status)
}

// ProgressResumeReturnRaw resumes a paused call (function or OS) with a
// return value, consuming the progress handle.
func ProgressResumeReturnRaw(progress uintptr, value *RawValue) (StepResult, error) {
	result, err := resumeStep(func(out *ProgressSnapshotOutput) uintptr {
		return syscall3(
			mgProgressResumeReturnRawAddr,
			progress,
			uintptr(ptrOf(value)),
			uintptr(ptrOf(out)),
		)
	})
	runtime.KeepAlive(value)
	return result, err
}

// ProgressResumeException resumes a paused state by raising an exception.
func ProgressResumeException(progress uintptr, excType, message string) (StepResult, error) {
	excRef := StringRef(excType)
	msgRef := StringRef(message)
	result, err := resumeStep(func(out *ProgressSnapshotOutput) uintptr {
		return syscall6(
			mgProgressResumeExceptionAddr,
			progress,
			uintptr(excRef.Ptr),
			excRef.Len,
			uintptr(msgRef.Ptr),
			msgRef.Len,
			uintptr(ptrOf(out)),
		)
	})
	runtime.KeepAlive(excType)
	runtime.KeepAlive(message)
	return result, err
}

// ProgressResumePending marks a paused function call as a pending future.
func ProgressResumePending(progress uintptr) (StepResult, error) {
	return resumeStep(func(out *ProgressSnapshotOutput) uintptr {
		return syscall2(mgProgressResumePendingAddr, progress, uintptr(ptrOf(out)))
	})
}

// ProgressResumeNotHandled resumes a paused OS call using Monty's default
// unhandled behavior (PermissionError for filesystem functions, RuntimeError
// otherwise).
func ProgressResumeNotHandled(progress uintptr) (StepResult, error) {
	return resumeStep(func(out *ProgressSnapshotOutput) uintptr {
		return syscall2(mgProgressResumeNotHandledAddr, progress, uintptr(ptrOf(out)))
	})
}

// ProgressResumeNameValueRaw resumes a name lookup with a value.
func ProgressResumeNameValueRaw(progress uintptr, value *RawValue) (StepResult, error) {
	result, err := resumeStep(func(out *ProgressSnapshotOutput) uintptr {
		return syscall3(
			mgProgressResumeNameValueRawAddr,
			progress,
			uintptr(ptrOf(value)),
			uintptr(ptrOf(out)),
		)
	})
	runtime.KeepAlive(value)
	return result, err
}

// ProgressResumeNameUndefined resumes a name lookup as undefined (Python
// raises a catchable NameError).
func ProgressResumeNameUndefined(progress uintptr) (StepResult, error) {
	return resumeStep(func(out *ProgressSnapshotOutput) uintptr {
		return syscall2(mgProgressResumeNameUndefinedAddr, progress, uintptr(ptrOf(out)))
	})
}

// ProgressResumeFutures resumes a futures-wait state with resolved results.
func ProgressResumeFutures(progress uintptr, results []FutureResult) (StepResult, error) {
	result, err := resumeStep(func(out *ProgressSnapshotOutput) uintptr {
		return syscall4(
			mgProgressResumeFuturesAddr,
			progress,
			sliceAddress(results),
			uintptr(len(results)),
			uintptr(ptrOf(out)),
		)
	})
	runtime.KeepAlive(results)
	return result, err
}

// ProgressDump serializes a progress handle.
func ProgressDump(progress uintptr) ([]byte, error) {
	var buffer Bytes
	var errHandle uintptr
	status := mgProgressDump(progress, ptrOf(&buffer), ptrOf(&errHandle))
	if status != StatusOK {
		return nil, TakeError(errHandle)
	}
	return TakeBytes(buffer), nil
}

// ProgressLoad restores a progress handle from a snapshot created by ProgressDump.
func ProgressLoad(snapshot []byte) (uintptr, error) {
	if err := EnsureLoaded(); err != nil {
		return 0, err
	}
	snapshotPtr, snapshotLen := BytesRef(snapshot)
	var progressHandle, errHandle uintptr
	status := mgProgressLoad(snapshotPtr, snapshotLen, ptrOf(&progressHandle), ptrOf(&errHandle))
	runtime.KeepAlive(snapshot)
	if status != StatusOK {
		return 0, TakeError(errHandle)
	}
	return progressHandle, nil
}
