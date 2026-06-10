package monty

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/joeychilson/monty/internal/ffi"
)

// Sentinel errors for binding-level misuse and limit classification. Limit
// sentinels match via errors.Is against the *ExecError a run returns.
var (
	// ErrClosed reports an operation on a closed (or nil) handle.
	ErrClosed = errors.New("monty: closed")
	// ErrBusy reports a REPL call while a Run started by REPL.Start is
	// still unfinished.
	ErrBusy = errors.New("monty: repl session is mid-snippet")
	// ErrPaused reports Run.Result called while the run is still paused.
	ErrPaused = errors.New("monty: run is paused")
	// ErrNotPaused reports Run.Dump called after the run finished.
	ErrNotPaused = errors.New("monty: run is not paused")
	// ErrResolved reports a resume on an interrupt that was already resolved.
	ErrResolved = errors.New("monty: interrupt already resolved")
	// ErrTimeLimit matches an ExecError caused by Limits.MaxDuration.
	ErrTimeLimit = errors.New("monty: time limit exceeded")
	// ErrMemoryLimit matches an ExecError caused by Limits.MaxMemory or
	// Limits.MaxAllocations.
	ErrMemoryLimit = errors.New("monty: memory limit exceeded")
	// ErrRecursionLimit matches an ExecError caused by Limits.MaxRecursionDepth.
	ErrRecursionLimit = errors.New("monty: recursion limit exceeded")
)

// Frame is one Python traceback frame, outermost first.
type Frame struct {
	// File is the script name the frame's code belongs to.
	File string
	// Line and Column are the 1-based start position.
	Line   int
	Column int
	// EndLine and EndColumn are the 1-based end position.
	EndLine   int
	EndColumn int
	// Function is the enclosing function name; empty for module-level code.
	Function string
	// SourceLine is the source text for traceback previews, when available.
	SourceLine string
}

// TracebackFormat selects how ExecError.Render formats the failure.
type TracebackFormat string

const (
	// FormatTraceback renders the full Python-style traceback.
	FormatTraceback TracebackFormat = "traceback"
	// FormatTypeMessage renders "ExceptionType: message".
	FormatTypeMessage TracebackFormat = "type-msg"
	// FormatMessage renders just the message.
	FormatMessage TracebackFormat = "msg"
)

// ExecError is a Python exception raised during execution (including resource
// limit violations, which surface as TimeoutError / MemoryError /
// RecursionError — match them with errors.Is against ErrTimeLimit,
// ErrMemoryLimit, and ErrRecursionLimit).
type ExecError struct {
	// Type is the Python exception type, such as "ValueError".
	Type string
	// Message is the exception message without the type prefix.
	Message string
	// Traceback holds the structured frames, outermost first. Empty for
	// failures with no Python frames.
	Traceback []Frame

	// display is the pre-rendered full traceback text from the runtime.
	display string
}

// Error returns "Type: message".
func (e *ExecError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return e.Type
	}
	return e.Type + ": " + e.Message
}

// Render formats the failure: the full traceback, "Type: message", or the
// bare message.
func (e *ExecError) Render(format TracebackFormat) string {
	if e == nil {
		return ""
	}
	switch format {
	case FormatMessage:
		return e.Message
	case FormatTypeMessage:
		return e.Error()
	default: // FormatTraceback
		if e.display != "" {
			return e.display
		}
		return e.Error()
	}
}

// Is matches the resource-limit sentinels against the Python exception type.
func (e *ExecError) Is(target error) bool {
	switch target {
	case ErrTimeLimit:
		return e.Type == "TimeoutError"
	case ErrMemoryLimit:
		return e.Type == "MemoryError"
	case ErrRecursionLimit:
		return e.Type == "RecursionError"
	default:
		return false
	}
}

// SyntaxError reports code that Monty could not parse or compile.
type SyntaxError struct {
	// Message is the parser message.
	Message string
	// Display is the formatted error with source context, when available.
	Display string
}

// Error returns the formatted syntax error.
func (e *SyntaxError) Error() string {
	if e == nil {
		return ""
	}
	if e.Display != "" {
		return e.Display
	}
	return "SyntaxError: " + e.Message
}

// Severity classifies one type-check diagnostic.
type Severity int

const (
	// SeverityError is a diagnostic that fails the check.
	SeverityError Severity = iota
	// SeverityWarning is a non-fatal diagnostic.
	SeverityWarning
	// SeverityInfo is an informational diagnostic.
	SeverityInfo
)

// String returns the lowercase severity name.
func (s Severity) String() string {
	switch s {
	case SeverityWarning:
		return "warning"
	case SeverityInfo:
		return "info"
	default:
		return "error"
	}
}

// Diagnostic is one structured type-check finding.
type Diagnostic struct {
	Severity  Severity
	Code      string
	Message   string
	File      string
	Line      int
	Column    int
	EndLine   int
	EndColumn int
}

// DiagnosticFormat selects a rendering of type-check diagnostics; the values
// mirror the formats the Monty type checker supports.
type DiagnosticFormat string

const (
	DiagnosticFull      DiagnosticFormat = "full"
	DiagnosticConcise   DiagnosticFormat = "concise"
	DiagnosticAzure     DiagnosticFormat = "azure"
	DiagnosticJSON      DiagnosticFormat = "json"
	DiagnosticJSONLines DiagnosticFormat = "jsonlines"
	DiagnosticRDJSON    DiagnosticFormat = "rdjson"
	DiagnosticPylint    DiagnosticFormat = "pylint"
	DiagnosticGitLab    DiagnosticFormat = "gitlab"
	DiagnosticGitHub    DiagnosticFormat = "github"
)

// TypeCheckError reports static type errors found before execution.
type TypeCheckError struct {
	// Diagnostics holds the structured findings.
	Diagnostics []Diagnostic

	summary string
	handle  *diagnosticsHandle
}

type diagnosticsHandle struct {
	handle  uintptr
	cleanup runtime.Cleanup
}

func newDiagnosticsHandle(handle uintptr) *diagnosticsHandle {
	h := &diagnosticsHandle{handle: handle}
	h.cleanup = runtime.AddCleanup(h, ffi.DiagnosticsFree, handle)
	return h
}

// Error returns the concise diagnostics rendering.
func (e *TypeCheckError) Error() string {
	if e == nil {
		return ""
	}
	if e.summary != "" {
		return strings.TrimRight(e.summary, "\n")
	}
	return fmt.Sprintf("monty: %d type errors", len(e.Diagnostics))
}

// Render formats the diagnostics in any supported format, optionally with
// ANSI colors. Errors carrying a live Rust handle render through the upstream
// formatter; remapped REPL errors (whose handle was released so coordinates
// could be shifted to snippet-relative space) render locally.
func (e *TypeCheckError) Render(format DiagnosticFormat, color bool) string {
	if e == nil {
		return ""
	}
	if e.handle != nil {
		rendered, err := ffi.DiagnosticsRender(e.handle.handle, string(format), color)
		runtime.KeepAlive(e.handle)
		if err == nil {
			return rendered
		}
	}
	return e.renderLocal(format)
}

// renderLocal formats remapped diagnostics without the upstream handle. JSON
// formats are reconstructed from the structured diagnostics; everything else
// falls back to the concise listing (the format the REPL surfaces).
func (e *TypeCheckError) renderLocal(format DiagnosticFormat) string {
	switch format {
	case DiagnosticJSON, DiagnosticJSONLines:
		return renderDiagnosticsJSON(e.Diagnostics, format == DiagnosticJSONLines)
	default:
		if e.summary != "" {
			return strings.TrimRight(e.summary, "\n")
		}
		return renderConcise(e.Diagnostics)
	}
}

// newTypeCheckError builds the public error from a Rust diagnostics handle,
// taking ownership of it.
func newTypeCheckError(handle uintptr) *TypeCheckError {
	result := &TypeCheckError{handle: newDiagnosticsHandle(handle)}
	if summary, err := ffi.DiagnosticsRender(handle, string(DiagnosticConcise), false); err == nil {
		result.summary = summary
	}
	if rendered, err := ffi.DiagnosticsRender(handle, string(DiagnosticJSON), false); err == nil {
		result.Diagnostics = parseDiagnosticsJSON(rendered)
	}
	runtime.KeepAlive(result.handle)
	return result
}

// parseDiagnosticsJSON tolerantly decodes the type checker's JSON rendering.
// Unknown fields are ignored so upstream schema drift degrades gracefully to
// fewer populated fields rather than an error.
func parseDiagnosticsJSON(data string) []Diagnostic {
	type location struct {
		Row    int `json:"row"`
		Column int `json:"column"`
	}
	type entry struct {
		Code        string    `json:"code"`
		Message     string    `json:"message"`
		Severity    string    `json:"severity"`
		Level       string    `json:"level"`
		Filename    string    `json:"filename"`
		File        string    `json:"file"`
		Location    *location `json:"location"`
		EndLocation *location `json:"end_location"`
	}
	var entries []entry
	if err := json.Unmarshal([]byte(data), &entries); err != nil {
		return nil
	}
	diagnostics := make([]Diagnostic, 0, len(entries))
	for _, item := range entries {
		diagnostic := Diagnostic{
			Code:    item.Code,
			Message: item.Message,
		}
		severity := item.Severity
		if severity == "" {
			severity = item.Level
		}
		switch strings.ToLower(severity) {
		case "warning":
			diagnostic.Severity = SeverityWarning
		case "info", "information", "note":
			diagnostic.Severity = SeverityInfo
		default:
			diagnostic.Severity = SeverityError
		}
		diagnostic.File = item.Filename
		if diagnostic.File == "" {
			diagnostic.File = item.File
		}
		if item.Location != nil {
			diagnostic.Line = item.Location.Row
			diagnostic.Column = item.Location.Column
		}
		if item.EndLocation != nil {
			diagnostic.EndLine = item.EndLocation.Row
			diagnostic.EndColumn = item.EndLocation.Column
		}
		diagnostics = append(diagnostics, diagnostic)
	}
	return diagnostics
}

// newTypeCheckErrorOffset builds the public error from a Rust diagnostics
// handle whose code was checked with offset leading lines of prepended context
// (REPL history). Coordinates are shifted back to snippet-relative space and
// diagnostics that fall inside the prepended region are dropped. It returns nil
// when no diagnostics survive the shift — meaning the snippet itself is clean.
//
// The coordinates baked into the upstream rendering cannot be shifted after the
// fact, so an offset error renders from its remapped structured diagnostics and
// the handle is released here rather than retained.
func newTypeCheckErrorOffset(handle uintptr, offset int) *TypeCheckError {
	if offset <= 0 {
		return newTypeCheckError(handle)
	}
	result := &TypeCheckError{}
	if rendered, err := ffi.DiagnosticsRender(handle, string(DiagnosticJSON), false); err == nil {
		result.Diagnostics = shiftDiagnostics(parseDiagnosticsJSON(rendered), offset)
	}
	ffi.DiagnosticsFree(handle)
	if len(result.Diagnostics) == 0 {
		return nil
	}
	result.summary = renderConcise(result.Diagnostics)
	return result
}

// shiftDiagnostics maps diagnostics from a module with offset leading context
// lines back to snippet-relative coordinates, discarding any that start inside
// the prepended region.
func shiftDiagnostics(diagnostics []Diagnostic, offset int) []Diagnostic {
	if offset <= 0 {
		return diagnostics
	}
	shifted := make([]Diagnostic, 0, len(diagnostics))
	for _, d := range diagnostics {
		if d.Line <= offset {
			continue
		}
		d.Line -= offset
		if d.EndLine > offset {
			d.EndLine -= offset
		} else {
			d.EndLine = 0
		}
		shifted = append(shifted, d)
	}
	return shifted
}

// renderConcise formats diagnostics as one "file:line:col: severity[code]
// message" line each, matching the upstream concise format.
func renderConcise(diagnostics []Diagnostic) string {
	lines := make([]string, len(diagnostics))
	for i, d := range diagnostics {
		lines[i] = fmt.Sprintf("%s:%d:%d: %s[%s] %s", d.File, d.Line, d.Column, d.Severity, d.Code, d.Message)
	}
	return strings.Join(lines, "\n")
}

// renderDiagnosticsJSON reconstructs the upstream JSON (or JSON Lines)
// rendering from structured diagnostics so remapped errors round-trip through
// parseDiagnosticsJSON.
func renderDiagnosticsJSON(diagnostics []Diagnostic, jsonLines bool) string {
	type location struct {
		Row    int `json:"row"`
		Column int `json:"column"`
	}
	type entry struct {
		Code        string   `json:"code"`
		Message     string   `json:"message"`
		Severity    string   `json:"severity"`
		Filename    string   `json:"filename"`
		Location    location `json:"location"`
		EndLocation location `json:"end_location"`
	}
	entries := make([]entry, len(diagnostics))
	for i, d := range diagnostics {
		entries[i] = entry{
			Code:        d.Code,
			Message:     d.Message,
			Severity:    d.Severity.String(),
			Filename:    d.File,
			Location:    location{Row: d.Line, Column: d.Column},
			EndLocation: location{Row: d.EndLine, Column: d.EndColumn},
		}
	}
	if jsonLines {
		var b strings.Builder
		for i := range entries {
			line, err := json.Marshal(entries[i])
			if err != nil {
				continue
			}
			b.Write(line)
			b.WriteByte('\n')
		}
		return b.String()
	}
	out, err := json.Marshal(entries)
	if err != nil {
		return "[]"
	}
	return string(out)
}

// --------------------------------------------------------------------------
// FFI error classification
// --------------------------------------------------------------------------

// execError converts an FFI failure into the public *ExecError taxonomy.
func execError(err error) error {
	if err == nil {
		return nil
	}
	if ffiErr, ok := errors.AsType[*ffi.Error](err); ok {
		return execErrorFromFFI(ffiErr)
	}
	return err
}

func execErrorFromFFI(ffiErr *ffi.Error) *ExecError {
	out := &ExecError{
		Type:    ffiErr.Type,
		Message: ffiErr.Message,
		display: ffiErr.Display,
	}
	if len(ffiErr.Traceback) != 0 {
		out.Traceback = make([]Frame, len(ffiErr.Traceback))
		for i, frame := range ffiErr.Traceback {
			out.Traceback[i] = Frame{
				File:       frame.File,
				Line:       frame.Line,
				Column:     frame.Column,
				EndLine:    frame.EndLine,
				EndColumn:  frame.EndColumn,
				Function:   frame.Function,
				SourceLine: frame.SourceLine,
			}
		}
	}
	return out
}

// compileError converts a compile-path FFI failure into *SyntaxError (for
// parser failures) or a generic error.
func compileError(err error) error {
	if err == nil {
		return nil
	}
	if ffiErr, ok := errors.AsType[*ffi.Error](err); ok {
		if ffiErr.Type == "SyntaxError" {
			return &SyntaxError{Message: ffiErr.Message, Display: ffiErr.Display}
		}
		return execErrorFromFFI(ffiErr)
	}
	return err
}

// normalizeError converts any *ffi.Error into the public taxonomy; other
// errors pass through unchanged.
func normalizeError(err error) error {
	return execError(err)
}

// exceptionFromError maps a Go error returned by host code to the Python
// exception that should be raised: typed exceptions keep their type, anything
// else raises RuntimeError with the error text.
func exceptionFromError(err error) (string, string) {
	const fallback = "RuntimeError"
	if err == nil {
		return fallback, ""
	}
	if exc, ok := errors.AsType[*Exception](err); ok && exc.Type != "" {
		return exc.Type, exc.Message
	}
	if execErr, ok := errors.AsType[*ExecError](err); ok && execErr.Type != "" {
		return execErr.Type, execErr.Message
	}
	return fallback, err.Error()
}
