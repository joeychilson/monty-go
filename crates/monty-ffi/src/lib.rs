//! C ABI for the Go binding of Monty, Pydantic's sandboxed Python interpreter.
//!
//! Conventions shared by every entry point:
//! - Functions return `STATUS_OK` (0) or `STATUS_ERR` (1). On error the
//!   trailing `out_error: *mut *mut MgError` (or the `error` field of the
//!   output struct) receives an owned `MgError` that the caller must release
//!   with `mg_error_free`. Resume entry points may instead return
//!   `STATUS_ERR_RETAINED` (2): the resume payload was rejected before the
//!   progress handle was consumed, so the caller still owns the live handle.
//! - Heap handles (`MgProgram`, `MgValue`, `MgProgress`, `MgRepl`, `MgMount`,
//!   `MgDiagnostics`, `MgCancelToken`) are created with `Box::into_raw` and
//!   released exactly once by the matching `mg_*_free` (or consumed by a call
//!   documented to do so).
//! - `MgBytes` outputs are Rust-owned allocations released by `mg_bytes_free`,
//!   except when documented to point into a caller-owned scratch buffer.
//! - The ABI is versioned: Go checks `mg_abi_version()` at load time.

#![allow(clippy::missing_safety_doc)]

mod error;
mod mount;
mod print;
mod program;
mod progress;
mod repl;
mod tracker;
mod typecheck;
mod value;

use std::{
    panic::{AssertUnwindSafe, catch_unwind},
    ptr, slice, str,
    sync::{Arc, Mutex},
    time::Duration,
};

use monty::fs::Mount as MontyMount;
use monty::{
    DictPairs, ExcType, MontyDate, MontyDateTime, MontyException, MontyObject, MontyRepl, MontyRun,
    MontyTimeDelta, MontyTimeZone, NoLimitTracker, ReplProgress, ResourceLimits, RunProgress,
};
use monty_type_checking::TypeCheckingDiagnostics;
use num_bigint::BigInt;
use serde::{Deserialize, Serialize};

pub(crate) use crate::print::PrintBuf;
pub(crate) use crate::tracker::GoTracker;

/// Incremented whenever any `#[repr(C)]` layout or entry-point contract
/// changes. The Go loader refuses to use a library with a mismatched version.
pub const MG_ABI_VERSION: u32 = 3;

#[unsafe(no_mangle)]
pub const extern "C" fn mg_abi_version() -> u32 {
    MG_ABI_VERSION
}

pub(crate) const STATUS_OK: i32 = 0;
pub(crate) const STATUS_ERR: i32 = 1;
/// The call failed before consuming the progress handle passed to it; the
/// caller still owns the handle and the paused state is retryable.
pub(crate) const STATUS_ERR_RETAINED: i32 = 2;

pub(crate) const FAST_FORMAT_RAW: u32 = 0;
pub(crate) const FAST_FORMAT_FLAT: u32 = 1;

pub(crate) const PROGRESS_FUNCTION_CALL: u32 = 1;
pub(crate) const PROGRESS_OS_CALL: u32 = 2;
pub(crate) const PROGRESS_RESOLVE_FUTURES: u32 = 3;
pub(crate) const PROGRESS_NAME_LOOKUP: u32 = 4;
pub(crate) const PROGRESS_COMPLETE: u32 = 5;

pub(crate) const FUTURE_RESULT_RETURN: u32 = 0;
pub(crate) const FUTURE_RESULT_ERROR: u32 = 1;
pub(crate) const FUTURE_RESULT_NOT_FOUND: u32 = 2;

/// Host callback verdicts.
pub(crate) const HOST_CALLBACK_RETURN: i32 = 0;
pub(crate) const HOST_CALLBACK_EXCEPTION: i32 = 1;
pub(crate) const HOST_CALLBACK_NOT_HANDLED: i32 = 2;

/// Host callback request kinds.
pub(crate) const HOST_CALL_FUNCTION: u32 = 1;
pub(crate) const HOST_CALL_OS: u32 = 2;

/// Print encoding in output structs: `print_flags == PRINT_PLAIN` means the
/// `print` buffer is raw stdout text; `PRINT_TAGGED` means it is a sequence of
/// `[u8 stream][u32 le len][len bytes]` chunks in emit order (stream 0 =
/// stdout, 1 = stderr).
pub(crate) const PRINT_PLAIN: u32 = 0;
pub(crate) const PRINT_TAGGED: u32 = 1;

pub(crate) const KIND_INVALID: u32 = 0;
pub(crate) const KIND_ELLIPSIS: u32 = 1;
pub(crate) const KIND_NONE: u32 = 2;
pub(crate) const KIND_BOOL: u32 = 3;
pub(crate) const KIND_INT: u32 = 4;
pub(crate) const KIND_BIG_INT: u32 = 5;
pub(crate) const KIND_FLOAT: u32 = 6;
pub(crate) const KIND_STRING: u32 = 7;
pub(crate) const KIND_BYTES: u32 = 8;
pub(crate) const KIND_LIST: u32 = 9;
pub(crate) const KIND_TUPLE: u32 = 10;
pub(crate) const KIND_NAMED_TUPLE: u32 = 11;
pub(crate) const KIND_DICT: u32 = 12;
pub(crate) const KIND_SET: u32 = 13;
pub(crate) const KIND_FROZEN_SET: u32 = 14;
pub(crate) const KIND_DATE: u32 = 15;
pub(crate) const KIND_DATETIME: u32 = 16;
pub(crate) const KIND_TIME_DELTA: u32 = 17;
pub(crate) const KIND_TIME_ZONE: u32 = 18;
pub(crate) const KIND_EXCEPTION: u32 = 19;
pub(crate) const KIND_TYPE: u32 = 20;
pub(crate) const KIND_BUILTIN_FUNCTION: u32 = 21;
pub(crate) const KIND_PATH: u32 = 22;
pub(crate) const KIND_DATACLASS: u32 = 23;
pub(crate) const KIND_FUNCTION: u32 = 24;
pub(crate) const KIND_REPR: u32 = 25;
pub(crate) const KIND_CYCLE: u32 = 26;
pub(crate) const KIND_OWNED_HANDLE: u32 = u32::MAX;

// ---------------------------------------------------------------------------
// C ABI structs
// ---------------------------------------------------------------------------

/// Borrowed string reference (Go-owned memory, valid for the call only).
#[repr(C)]
#[derive(Clone, Copy)]
pub struct MgStr {
    pub(crate) ptr: *const u8,
    pub(crate) len: usize,
}

impl MgStr {
    pub(crate) const fn empty() -> Self {
        Self {
            ptr: ptr::null(),
            len: 0,
        }
    }
}

/// Rust-owned byte buffer handed to the caller; release with `mg_bytes_free`.
#[repr(C)]
pub struct MgBytes {
    pub(crate) ptr: *mut u8,
    pub(crate) len: usize,
}

impl MgBytes {
    pub(crate) const fn empty() -> Self {
        Self {
            ptr: ptr::null_mut(),
            len: 0,
        }
    }
}

/// Resource limits plus an optional cancellation token.
///
/// `cancel_token` (nullable) attaches a cancellation flag to the execution's
/// resource tracker: `mg_cancel_token_cancel` aborts the run at the next
/// statement boundary.
#[repr(C)]
pub struct MgLimits {
    pub(crate) max_allocations_set: u8,
    pub(crate) max_allocations: usize,
    pub(crate) max_duration_nanos_set: u8,
    pub(crate) max_duration_nanos: u64,
    pub(crate) max_memory_set: u8,
    pub(crate) max_memory: usize,
    pub(crate) gc_interval_set: u8,
    pub(crate) gc_interval: usize,
    pub(crate) max_recursion_depth_set: u8,
    pub(crate) max_recursion_depth: usize,
    pub(crate) disable_recursion_limit: u8,
    pub(crate) cancel_token: *mut MgCancelToken,
}

#[repr(C)]
pub struct MgProgramCompileArgs {
    pub(crate) code: MgStr,
    pub(crate) script_name: MgStr,
    pub(crate) input_names: *const MgStr,
    pub(crate) input_count: usize,
}

#[repr(C)]
pub struct MgCompileRunFastRawArgs {
    pub(crate) code: MgStr,
    pub(crate) script_name: MgStr,
    pub(crate) input_names: *const MgStr,
    pub(crate) input_count: usize,
    pub(crate) input_values: *mut MgRawValue,
    pub(crate) input_value_count: usize,
    pub(crate) limits: *const MgLimits,
}

/// Unified host-dispatch callback.
///
/// `kind` is `HOST_CALL_FUNCTION` for external function calls and
/// `HOST_CALL_OS` for OS calls. Return values are `HOST_CALLBACK_RETURN`,
/// `HOST_CALLBACK_EXCEPTION`, or `HOST_CALLBACK_NOT_HANDLED` (OS calls only:
/// fall back to Monty's default unhandled behavior).
pub type MgHostCallCallback = Option<
    unsafe extern "C" fn(
        user_data: usize,
        kind: u32,
        name_ptr: *const u8,
        name_len: usize,
        args: *const MgRawValue,
        kwargs: *const MgRawValue,
        out: *mut MgHostFunctionOutput,
    ) -> i32,
>;

/// Streaming print callback: receives each flushed output fragment during
/// execution. `stream` is 0 for stdout, 1 for stderr.
pub type MgPrintCallback =
    Option<unsafe extern "C" fn(user_data: usize, stream: u8, ptr: *const u8, len: usize)>;

/// Arguments for single-hop host-dispatch program runs.
#[repr(C)]
pub struct MgRunHostArgs {
    pub(crate) inputs: *mut MgRawValue,
    pub(crate) input_count: usize,
    pub(crate) limits: *const MgLimits,
    pub(crate) host_names: *const MgStr,
    pub(crate) host_name_count: usize,
    pub(crate) mounts: *const *mut MgMount,
    pub(crate) mount_count: usize,
    pub(crate) callback: MgHostCallCallback,
    pub(crate) callback_data: usize,
    pub(crate) print: MgPrintCallback,
    pub(crate) print_data: usize,
}

#[repr(C)]
pub struct MgMountNewArgs {
    pub(crate) virtual_path: MgStr,
    pub(crate) host_path: MgStr,
    pub(crate) mode: u32,
    pub(crate) has_write_bytes_limit: u8,
    pub(crate) _pad: [u8; 3],
    pub(crate) write_bytes_limit: u64,
}

#[repr(C)]
pub struct MgMountCallArgs {
    pub(crate) mounts: *const *mut MgMount,
    pub(crate) mount_count: usize,
    pub(crate) function: MgStr,
    pub(crate) args: *mut MgRawValue,
    pub(crate) arg_count: usize,
    pub(crate) kwargs: *mut MgRawPair,
    pub(crate) kwarg_count: usize,
}

#[repr(C)]
pub struct MgMountOutput {
    pub(crate) value: MgRawValue,
    pub(crate) error: *mut MgError,
    pub(crate) handled: u8,
}

#[repr(C)]
pub struct MgReplNewArgs {
    pub(crate) script_name: MgStr,
    pub(crate) limits: *const MgLimits,
}

/// Arguments shared by `mg_repl_feed_run_raw` and `mg_repl_feed_start`.
///
/// `max_duration_nanos` (when `has_max_duration` is set) resets the REPL
/// tracker's time budget for this snippet; it requires a REPL constructed with
/// limits. `cancel_token` attaches per-snippet cancellation the same way.
/// `host_names`/`mounts`/`callback` are honored by `mg_repl_feed_run_raw` only
/// and must be empty/null for `mg_repl_feed_start`.
#[repr(C)]
pub struct MgReplFeedArgs {
    pub(crate) code: MgStr,
    pub(crate) input_names: *const MgStr,
    pub(crate) input_values: *mut MgRawValue,
    pub(crate) input_count: usize,
    pub(crate) has_max_duration: u8,
    pub(crate) _pad: [u8; 7],
    pub(crate) max_duration_nanos: u64,
    pub(crate) cancel_token: *mut MgCancelToken,
    pub(crate) host_names: *const MgStr,
    pub(crate) host_name_count: usize,
    pub(crate) mounts: *const *mut MgMount,
    pub(crate) mount_count: usize,
    pub(crate) callback: MgHostCallCallback,
    pub(crate) callback_data: usize,
    pub(crate) print: MgPrintCallback,
    pub(crate) print_data: usize,
}

#[repr(C)]
pub struct MgReplCallArgs {
    pub(crate) name: MgStr,
    pub(crate) args: *mut MgRawValue,
    pub(crate) arg_count: usize,
}

#[repr(C)]
pub struct MgTypeCheckArgs {
    pub(crate) code: MgStr,
    pub(crate) script_name: MgStr,
    pub(crate) stubs: MgStr,
    pub(crate) stubs_name: MgStr,
}

#[repr(C)]
pub struct MgFutureResult {
    pub(crate) call_id: u32,
    pub(crate) kind: u32,
    pub(crate) value: MgRawValue,
    pub(crate) exc_type: MgStr,
    pub(crate) message: MgStr,
}

/// A Python value crossing the FFI boundary without a heap handle when possible.
///
/// Scalars are inline; strings/bytes point at borrowed or Rust-owned
/// buffers; containers point at recursive `MgRawValue`/`MgRawPair` arrays;
/// everything else is an owned `MgValue` handle (`kind == KIND_OWNED_HANDLE`
/// for Go-owned input handles, or `handle != null` for Rust-produced output).
#[repr(C)]
pub struct MgRawValue {
    pub(crate) kind: u32,
    pub(crate) bool_value: u8,
    pub(crate) _pad: [u8; 3],
    pub(crate) int_value: i64,
    pub(crate) float_value: f64,
    pub(crate) ptr: *mut u8,
    pub(crate) len: usize,
    pub(crate) handle: *mut MgValue,
}

#[repr(C)]
pub struct MgRawPair {
    pub(crate) key: MgRawValue,
    pub(crate) value: MgRawValue,
}

/// Inline scratch capacity used for flat-format result bytes. Sized to cover
/// every value produced by the benchmark suite (records-of-100 lands around
/// 3 KiB) so the Go caller never needs a second cgocall to free a heap buffer.
pub(crate) const FAST_SCRATCH_CAP: usize = 8192;

#[repr(C)]
pub struct MgRunFastOutput {
    pub(crate) format: u32,
    /// Set to 1 when `bytes.ptr` points into `scratch` (Go-owned, no free
    /// required); 0 when `bytes` points at a Rust-owned heap allocation that
    /// must be released with `mg_bytes_free`.
    pub(crate) bytes_in_scratch: u32,
    /// `PRINT_PLAIN` or `PRINT_TAGGED`; see the constants for the encoding.
    pub(crate) print_flags: u32,
    pub(crate) _pad: u32,
    pub(crate) value: MgRawValue,
    pub(crate) bytes: MgBytes,
    pub(crate) print: MgBytes,
    pub(crate) error: *mut MgError,
    /// Caller-owned scratch buffer. Filled by the Rust side when a flat-encoded
    /// result fits; otherwise the Rust side allocates `bytes` separately and
    /// leaves `scratch` untouched.
    pub(crate) scratch: [u8; FAST_SCRATCH_CAP],
}

#[repr(C)]
pub struct MgRunJsonOutput {
    pub(crate) value: MgBytes,
    pub(crate) print: MgBytes,
    pub(crate) print_flags: u32,
    pub(crate) _pad: u32,
    pub(crate) error: *mut MgError,
}

#[repr(C)]
pub struct MgProgressSnapshot {
    pub(crate) kind: u32,
    pub(crate) call_id: u32,
    pub(crate) method_call: u8,
    pub(crate) _pad: [u8; 7],
    pub(crate) name: MgBytes,
    pub(crate) args: MgRawValue,
    pub(crate) kwargs: MgRawValue,
    pub(crate) value: MgRawValue,
}

/// Result of starting or resuming a suspendable execution.
///
/// `progress` is null when the run completed or failed. `repl` is non-null
/// when a REPL-variant execution handed its session back: at `Complete`, or
/// alongside `error` when a Python exception preserved the session
/// (`ReplStartError`). The caller owns both handles.
#[repr(C)]
pub struct MgProgressSnapshotOutput {
    pub(crate) progress: *mut MgProgress,
    pub(crate) repl: *mut MgRepl,
    pub(crate) error: *mut MgError,
    pub(crate) print: MgBytes,
    pub(crate) print_flags: u32,
    pub(crate) _pad: u32,
    pub(crate) snapshot: MgProgressSnapshot,
}

#[repr(C)]
pub struct MgHostFunctionOutput {
    pub(crate) value: MgRawValue,
    pub(crate) exc_type: MgStr,
    pub(crate) message: MgStr,
}

#[repr(C)]
#[derive(Clone, Copy)]
pub struct MgDate {
    pub(crate) year: i32,
    pub(crate) month: u8,
    pub(crate) day: u8,
    pub(crate) _pad: [u8; 2],
}

#[repr(C)]
pub struct MgDateTime {
    pub(crate) timezone_name: MgBytes,
    pub(crate) year: i32,
    pub(crate) microsecond: u32,
    pub(crate) offset_seconds: i32,
    pub(crate) month: u8,
    pub(crate) day: u8,
    pub(crate) hour: u8,
    pub(crate) minute: u8,
    pub(crate) second: u8,
    pub(crate) has_offset: u8,
    pub(crate) has_timezone_name: u8,
    pub(crate) _pad: u8,
}

#[repr(C)]
#[derive(Clone, Copy)]
pub struct MgTimeDelta {
    pub(crate) days: i32,
    pub(crate) seconds: i32,
    pub(crate) microseconds: i32,
}

#[repr(C)]
pub struct MgTimeZone {
    pub(crate) name: MgBytes,
    pub(crate) offset_seconds: i32,
    pub(crate) has_name: u8,
    pub(crate) _pad: [u8; 3],
}

#[repr(C)]
pub struct MgDataclassRawArgs {
    pub(crate) name: MgStr,
    pub(crate) type_id: u64,
    pub(crate) field_names: *const MgStr,
    pub(crate) field_count: usize,
    pub(crate) attrs: *mut MgRawPair,
    pub(crate) attr_count: usize,
    pub(crate) frozen: u8,
    pub(crate) _pad: [u8; 7],
}

// ---------------------------------------------------------------------------
// Handle types
// ---------------------------------------------------------------------------

pub struct MgProgram {
    pub(crate) runner: MontyRun,
    pub(crate) script_name: String,
    pub(crate) input_names: Vec<String>,
}

pub struct MgValue {
    pub(crate) object: MontyObject,
}

/// FFI error handed to the Go side.
///
/// The precomputed strings cover the common path; `exc` retains the full
/// Monty exception so tracebacks stay available without re-running. Boxed to
/// keep `Result<_, MgError>` small on every fallible internal path.
#[derive(Debug)]
pub struct MgError {
    pub(crate) exc_type: String,
    pub(crate) message: String,
    pub(crate) display: String,
    pub(crate) exc: Option<Box<MontyException>>,
}

pub struct MgMount {
    pub(crate) slot: Arc<Mutex<Option<MontyMount>>>,
}

/// Structured type-check diagnostics. The inner value moves out and back
/// during rendering because the upstream builder methods consume `self`.
pub struct MgDiagnostics {
    pub(crate) inner: Mutex<Option<TypeCheckingDiagnostics>>,
}

/// Thread-safe cancellation flag shared with one or more resource trackers.
pub struct MgCancelToken(pub(crate) Arc<std::sync::atomic::AtomicBool>);

/// Suspendable execution state: program or REPL, with or without limits.
///
/// Postcard variant indices are part of the snapshot format.
#[derive(Serialize, Deserialize)]
pub enum MgProgress {
    NoLimit(RunProgress<NoLimitTracker>),
    Limited(RunProgress<GoTracker>),
    ReplNoLimit(ReplProgress<NoLimitTracker>),
    ReplLimited(ReplProgress<GoTracker>),
}

#[derive(Serialize, Deserialize)]
pub(crate) enum ReplInner {
    NoLimit(MontyRepl<NoLimitTracker>),
    Limited(MontyRepl<GoTracker>),
}

/// A REPL session slot. `feed_start` consumes the session (it moves into the
/// returned progress); the slot is left empty so later use of a consumed
/// handle fails loudly instead of corrupting memory.
pub struct MgRepl(pub(crate) Option<ReplInner>);

#[derive(Serialize, Deserialize)]
pub(crate) struct StoredProgram {
    pub(crate) runner: MontyRun,
    pub(crate) script_name: String,
    pub(crate) input_names: Vec<String>,
}

// ---------------------------------------------------------------------------
// Error helpers
// ---------------------------------------------------------------------------

pub(crate) fn ffi_error(exc_type: impl Into<String>, message: impl Into<String>) -> MgError {
    let exc_type = exc_type.into();
    let message = message.into();
    MgError {
        display: if message.is_empty() {
            exc_type.clone()
        } else {
            format!("{exc_type}: {message}")
        },
        exc_type,
        message,
        exc: None,
    }
}

pub(crate) fn from_monty_error(exc: MontyException) -> MgError {
    MgError {
        exc_type: exc.exc_type().to_string(),
        message: exc.message().unwrap_or_default().to_owned(),
        display: exc.to_string(),
        exc: Some(Box::new(exc)),
    }
}

pub(crate) fn set_error(out_error: *mut *mut MgError, error: MgError) {
    if !out_error.is_null() {
        // SAFETY: out_error is provided by the caller and checked for null above.
        unsafe { *out_error = Box::into_raw(Box::new(error)) };
    }
}

pub(crate) fn guard(out_error: *mut *mut MgError, f: impl FnOnce() -> Result<(), MgError>) -> i32 {
    match catch_unwind(AssertUnwindSafe(f)) {
        Ok(Ok(())) => STATUS_OK,
        Ok(Err(error)) => {
            set_error(out_error, error);
            STATUS_ERR
        }
        Err(_) => {
            set_error(
                out_error,
                ffi_error("Panic", "Rust panic crossed the Monty Go FFI boundary"),
            );
            STATUS_ERR
        }
    }
}

// ---------------------------------------------------------------------------
// Input/output helpers
// ---------------------------------------------------------------------------

pub(crate) fn as_str(input: MgStr) -> Result<&'static str, MgError> {
    if input.ptr.is_null() {
        if input.len == 0 {
            return Ok("");
        }
        return Err(ffi_error("TypeError", "non-empty string has null pointer"));
    }
    // SAFETY: caller promises ptr points to len bytes that live for this FFI call.
    let bytes = unsafe { slice::from_raw_parts(input.ptr, input.len) };
    str::from_utf8(bytes).map_err(|err| ffi_error("UnicodeDecodeError", err.to_string()))
}

pub(crate) fn str_arg(ptr: *const u8, len: usize) -> Result<&'static str, MgError> {
    as_str(MgStr { ptr, len })
}

pub(crate) fn as_bytes<'a>(ptr: *const u8, len: usize) -> Result<&'a [u8], MgError> {
    if ptr.is_null() {
        if len == 0 {
            return Ok(&[]);
        }
        return Err(ffi_error(
            "TypeError",
            "non-empty byte slice has null pointer",
        ));
    }
    // SAFETY: caller promises ptr points to len bytes that live for this FFI call.
    Ok(unsafe { slice::from_raw_parts(ptr, len) })
}

pub(crate) fn write_bytes(out: *mut MgBytes, bytes: &[u8]) -> Result<(), MgError> {
    if out.is_null() {
        return Err(ffi_error("TypeError", "output bytes pointer is null"));
    }
    if bytes.is_empty() {
        // SAFETY: out is checked for null above and receives an empty buffer.
        unsafe { *out = MgBytes::empty() };
        return Ok(());
    }
    let owned = bytes.to_vec().into_boxed_slice();
    let leaked = Box::leak(owned);
    let len = leaked.len();
    let ptr = leaked.as_mut_ptr();
    // SAFETY: out is checked for null above and receives an owned allocation.
    unsafe { *out = MgBytes { ptr, len } };
    Ok(())
}

pub(crate) fn write_owned_bytes(out: *mut MgBytes, mut bytes: Vec<u8>) -> Result<(), MgError> {
    if out.is_null() {
        return Err(ffi_error("TypeError", "output bytes pointer is null"));
    }
    if bytes.is_empty() {
        // SAFETY: out is checked for null above and receives an empty buffer.
        unsafe { *out = MgBytes::empty() };
        return Ok(());
    }
    bytes.shrink_to_fit();
    let owned = bytes.into_boxed_slice();
    let leaked = Box::leak(owned);
    let len = leaked.len();
    let ptr = leaked.as_mut_ptr();
    // SAFETY: out is checked for null above and receives an owned allocation.
    unsafe { *out = MgBytes { ptr, len } };
    Ok(())
}

pub(crate) fn write_string(out: *mut MgBytes, value: &str) -> Result<(), MgError> {
    write_bytes(out, value.as_bytes())
}

pub(crate) fn write_owned_string(out: *mut MgBytes, value: String) -> Result<(), MgError> {
    write_owned_bytes(out, value.into_bytes())
}

pub(crate) fn read_value(value: *const MgValue) -> Result<MontyObject, MgError> {
    if value.is_null() {
        return Err(ffi_error("TypeError", "value handle is null"));
    }
    // SAFETY: handle validity is owned by the Go side contract.
    Ok(unsafe { (*value).object.clone() })
}

pub(crate) fn read_string_list(ptr: *const MgStr, len: usize) -> Result<Vec<String>, MgError> {
    if ptr.is_null() {
        if len == 0 {
            return Ok(Vec::new());
        }
        return Err(ffi_error(
            "TypeError",
            "non-empty string array has null pointer",
        ));
    }
    // SAFETY: caller promises ptr points to len MgStr entries.
    unsafe { slice::from_raw_parts(ptr, len) }
        .iter()
        .map(|value| as_str(*value).map(str::to_owned))
        .collect()
}

/// Borrowed variant of `read_string_list` for strings that only need to live
/// for the duration of the FFI call — avoids one allocation per entry.
pub(crate) fn read_borrowed_str_list(
    ptr: *const MgStr,
    len: usize,
) -> Result<Vec<&'static str>, MgError> {
    if ptr.is_null() {
        if len == 0 {
            return Ok(Vec::new());
        }
        return Err(ffi_error(
            "TypeError",
            "non-empty string array has null pointer",
        ));
    }
    // SAFETY: caller promises ptr points to len MgStr entries.
    unsafe { slice::from_raw_parts(ptr, len) }
        .iter()
        .map(|value| as_str(*value))
        .collect()
}

pub(crate) fn string_list_value(values: &[String]) -> *mut MgValue {
    let items = values
        .iter()
        .map(|value| MontyObject::String(value.clone()))
        .collect();
    Box::into_raw(Box::new(MgValue {
        object: MontyObject::List(items),
    }))
}

pub(crate) const fn monty_date_from_raw(raw: MgDate) -> MontyDate {
    MontyDate {
        year: raw.year,
        month: raw.month,
        day: raw.day,
    }
}

pub(crate) fn monty_datetime_from_raw(raw: &MgDateTime) -> Result<MontyDateTime, MgError> {
    let timezone_name = if raw.has_timezone_name != 0 {
        Some(str_arg(raw.timezone_name.ptr.cast_const(), raw.timezone_name.len)?.to_owned())
    } else {
        None
    };
    Ok(MontyDateTime {
        year: raw.year,
        month: raw.month,
        day: raw.day,
        hour: raw.hour,
        minute: raw.minute,
        second: raw.second,
        microsecond: raw.microsecond,
        offset_seconds: (raw.has_offset != 0).then_some(raw.offset_seconds),
        timezone_name,
    })
}

pub(crate) const fn monty_timedelta_from_raw(raw: MgTimeDelta) -> MontyTimeDelta {
    MontyTimeDelta {
        days: raw.days,
        seconds: raw.seconds,
        microseconds: raw.microseconds,
    }
}

pub(crate) fn monty_timezone_from_raw(raw: &MgTimeZone) -> Result<MontyTimeZone, MgError> {
    let name = if raw.has_name != 0 {
        Some(str_arg(raw.name.ptr.cast_const(), raw.name.len)?.to_owned())
    } else {
        None
    };
    Ok(MontyTimeZone {
        offset_seconds: raw.offset_seconds,
        name,
    })
}

// ---------------------------------------------------------------------------
// Raw value encode/decode
// ---------------------------------------------------------------------------

pub(crate) const fn raw_value(kind: u32) -> MgRawValue {
    MgRawValue {
        kind,
        bool_value: 0,
        _pad: [0; 3],
        int_value: 0,
        float_value: 0.0,
        ptr: ptr::null_mut(),
        len: 0,
        handle: ptr::null_mut(),
    }
}

pub(crate) fn raw_bytes(kind: u32, bytes: &[u8]) -> MgRawValue {
    let mut raw = raw_value(kind);
    if bytes.is_empty() {
        return raw;
    }
    let owned = bytes.to_vec().into_boxed_slice();
    let leaked = Box::leak(owned);
    raw.len = leaked.len();
    raw.ptr = leaked.as_mut_ptr();
    raw
}

#[allow(clippy::cast_ptr_alignment)]
pub(crate) const unsafe fn raw_value_slice_mut<'a>(
    ptr: *mut u8,
    len: usize,
) -> &'a mut [MgRawValue] {
    // SAFETY: callers only pass pointers that were originally allocated as
    // MgRawValue arrays by this crate or received from Go's matching ABI type.
    unsafe { slice::from_raw_parts_mut(ptr.cast::<MgRawValue>(), len) }
}

#[allow(clippy::cast_ptr_alignment)]
pub(crate) const unsafe fn raw_pair_slice_mut<'a>(ptr: *mut u8, len: usize) -> &'a mut [MgRawPair] {
    // SAFETY: callers only pass pointers that were originally allocated as
    // MgRawPair arrays by this crate or received from Go's matching ABI type.
    unsafe { slice::from_raw_parts_mut(ptr.cast::<MgRawPair>(), len) }
}

#[allow(clippy::cast_ptr_alignment)]
pub(crate) unsafe fn raw_value_box_from_raw(ptr: *mut u8, len: usize) -> Box<[MgRawValue]> {
    // SAFETY: ptr/len must come from raw_sequence, which allocated a boxed
    // MgRawValue slice and stored its data pointer in MgRawValue.ptr.
    unsafe { Box::from_raw(ptr::slice_from_raw_parts_mut(ptr.cast::<MgRawValue>(), len)) }
}

#[allow(clippy::cast_ptr_alignment)]
pub(crate) unsafe fn raw_pair_box_from_raw(ptr: *mut u8, len: usize) -> Box<[MgRawPair]> {
    // SAFETY: ptr/len must come from raw_dict, which allocated a boxed
    // MgRawPair slice and stored its data pointer in MgRawValue.ptr.
    unsafe { Box::from_raw(ptr::slice_from_raw_parts_mut(ptr.cast::<MgRawPair>(), len)) }
}

pub(crate) fn raw_sequence(kind: u32, values: Vec<MontyObject>) -> Result<MgRawValue, MgError> {
    let mut raw = raw_value(kind);
    if values.is_empty() {
        return Ok(raw);
    }
    let mut items = Vec::with_capacity(values.len());
    for value in values {
        let mut item = raw_value(KIND_INVALID);
        if let Err(error) = write_raw_value(ptr::addr_of_mut!(item), value) {
            for item in &mut items {
                free_raw_value(item);
            }
            return Err(error);
        }
        items.push(item);
    }
    let boxed = items.into_boxed_slice();
    let leaked = Box::leak(boxed);
    raw.len = leaked.len();
    raw.ptr = leaked.as_mut_ptr().cast();
    Ok(raw)
}

pub(crate) fn raw_dict(kind: u32, pairs: DictPairs) -> Result<MgRawValue, MgError> {
    let mut raw = raw_value(kind);
    if pairs.is_empty() {
        return Ok(raw);
    }
    let mut items = Vec::with_capacity(pairs.len());
    for (key, value) in pairs {
        let mut pair = MgRawPair {
            key: raw_value(KIND_INVALID),
            value: raw_value(KIND_INVALID),
        };
        if let Err(error) = write_raw_value(ptr::addr_of_mut!(pair.key), key) {
            for pair in &mut items {
                free_raw_pair(pair);
            }
            return Err(error);
        }
        if let Err(error) = write_raw_value(ptr::addr_of_mut!(pair.value), value) {
            free_raw_value(&mut pair.key);
            for pair in &mut items {
                free_raw_pair(pair);
            }
            return Err(error);
        }
        items.push(pair);
    }
    let boxed = items.into_boxed_slice();
    let leaked = Box::leak(boxed);
    raw.len = leaked.len();
    raw.ptr = leaked.as_mut_ptr().cast();
    Ok(raw)
}

// ---------------------------------------------------------------------------
// Flat encoding (compound results without per-node allocations)
// ---------------------------------------------------------------------------

pub(crate) fn push_flat_u32(out: &mut Vec<u8>, value: usize) -> Result<(), MgError> {
    let value = u32::try_from(value)
        .map_err(|_| ffi_error("OverflowError", "flat value length exceeds u32"))?;
    out.extend_from_slice(&value.to_le_bytes());
    Ok(())
}

pub(crate) fn push_flat_bytes(out: &mut Vec<u8>, bytes: &[u8]) -> Result<(), MgError> {
    push_flat_u32(out, bytes.len())?;
    out.extend_from_slice(bytes);
    Ok(())
}

pub(crate) fn write_flat_value(out: &mut Vec<u8>, object: &MontyObject) -> Result<(), MgError> {
    out.extend_from_slice(&object_kind(object).to_le_bytes());
    match object {
        MontyObject::Ellipsis | MontyObject::None => Ok(()),
        MontyObject::Bool(value) => {
            out.push(u8::from(*value));
            Ok(())
        }
        MontyObject::Int(value) => {
            out.extend_from_slice(&value.to_le_bytes());
            Ok(())
        }
        MontyObject::BigInt(value) => push_flat_bytes(out, value.to_string().as_bytes()),
        MontyObject::Float(value) => {
            out.extend_from_slice(&value.to_le_bytes());
            Ok(())
        }
        MontyObject::String(value) => push_flat_bytes(out, value.as_bytes()),
        MontyObject::Bytes(value) => push_flat_bytes(out, value),
        MontyObject::List(values)
        | MontyObject::Tuple(values)
        | MontyObject::Set(values)
        | MontyObject::FrozenSet(values) => {
            push_flat_u32(out, values.len())?;
            for value in values {
                write_flat_value(out, value)?;
            }
            Ok(())
        }
        MontyObject::Dict(pairs) => {
            push_flat_u32(out, pairs.len())?;
            for (key, value) in pairs {
                write_flat_value(out, key)?;
                write_flat_value(out, value)?;
            }
            Ok(())
        }
        _ => Err(ffi_error(
            "UnsupportedType",
            "value is not supported by flat FFI",
        )),
    }
}

pub(crate) const fn object_kind(object: &MontyObject) -> u32 {
    match object {
        MontyObject::Ellipsis => KIND_ELLIPSIS,
        MontyObject::None => KIND_NONE,
        MontyObject::Bool(_) => KIND_BOOL,
        MontyObject::Int(_) => KIND_INT,
        MontyObject::BigInt(_) => KIND_BIG_INT,
        MontyObject::Float(_) => KIND_FLOAT,
        MontyObject::String(_) => KIND_STRING,
        MontyObject::Bytes(_) => KIND_BYTES,
        MontyObject::List(_) => KIND_LIST,
        MontyObject::Tuple(_) => KIND_TUPLE,
        MontyObject::NamedTuple { .. } => KIND_NAMED_TUPLE,
        MontyObject::Dict(_) => KIND_DICT,
        MontyObject::Set(_) => KIND_SET,
        MontyObject::FrozenSet(_) => KIND_FROZEN_SET,
        MontyObject::Date(_) => KIND_DATE,
        MontyObject::DateTime(_) => KIND_DATETIME,
        MontyObject::TimeDelta(_) => KIND_TIME_DELTA,
        MontyObject::TimeZone(_) => KIND_TIME_ZONE,
        MontyObject::Exception { .. } => KIND_EXCEPTION,
        MontyObject::Type(_) => KIND_TYPE,
        MontyObject::BuiltinFunction(_) => KIND_BUILTIN_FUNCTION,
        MontyObject::Path(_) => KIND_PATH,
        MontyObject::Dataclass { .. } => KIND_DATACLASS,
        MontyObject::Function { .. } => KIND_FUNCTION,
        MontyObject::Repr(_) => KIND_REPR,
        MontyObject::Cycle(_, _) => KIND_CYCLE,
    }
}

pub(crate) fn read_raw_value(value: &mut MgRawValue) -> Result<MontyObject, MgError> {
    match value.kind {
        KIND_ELLIPSIS => Ok(MontyObject::Ellipsis),
        KIND_NONE => Ok(MontyObject::None),
        KIND_BOOL => Ok(MontyObject::Bool(value.bool_value != 0)),
        KIND_INT => Ok(MontyObject::Int(value.int_value)),
        KIND_BIG_INT => {
            let integer = str_arg(value.ptr.cast_const(), value.len)?
                .parse::<BigInt>()
                .map_err(|err| ffi_error("ValueError", err.to_string()))?;
            Ok(MontyObject::BigInt(integer))
        }
        KIND_FLOAT => Ok(MontyObject::Float(value.float_value)),
        KIND_STRING => Ok(MontyObject::String(
            str_arg(value.ptr.cast_const(), value.len)?.to_owned(),
        )),
        KIND_FUNCTION => Ok(MontyObject::Function {
            name: str_arg(value.ptr.cast_const(), value.len)?.to_owned(),
            docstring: None,
        }),
        KIND_BYTES => Ok(MontyObject::Bytes(
            as_bytes(value.ptr.cast_const(), value.len)?.to_vec(),
        )),
        KIND_LIST if value.handle.is_null() => {
            read_raw_values_from_bytes(value.ptr, value.len).map(MontyObject::List)
        }
        KIND_TUPLE if value.handle.is_null() => {
            read_raw_values_from_bytes(value.ptr, value.len).map(MontyObject::Tuple)
        }
        KIND_SET if value.handle.is_null() => {
            read_raw_values_from_bytes(value.ptr, value.len).map(MontyObject::Set)
        }
        KIND_FROZEN_SET if value.handle.is_null() => {
            read_raw_values_from_bytes(value.ptr, value.len).map(MontyObject::FrozenSet)
        }
        KIND_DICT if value.handle.is_null() => read_raw_pairs_from_bytes(value.ptr, value.len)
            .map(Into::into)
            .map(MontyObject::Dict),
        KIND_OWNED_HANDLE => take_owned_raw_handle(value),
        _ if !value.handle.is_null() => read_value(value.handle),
        _ => Err(ffi_error(
            "TypeError",
            format!("raw value kind {} is not supported", value.kind),
        )),
    }
}

pub(crate) fn take_owned_raw_handle(value: &mut MgRawValue) -> Result<MontyObject, MgError> {
    if value.handle.is_null() {
        return Err(ffi_error("TypeError", "owned raw value handle is null"));
    }
    let handle = value.handle;
    value.handle = ptr::null_mut();
    value.kind = KIND_INVALID;
    // SAFETY: owned raw handles are allocated with Box::into_raw in this crate
    // and are consumed exactly once here.
    let boxed = unsafe { Box::from_raw(handle) };
    Ok(boxed.object)
}

pub(crate) fn free_owned_raw_handle(value: &mut MgRawValue) {
    match value.kind {
        KIND_OWNED_HANDLE if !value.handle.is_null() => {
            // SAFETY: owned raw handles are allocated with Box::into_raw in this crate.
            unsafe { drop(Box::from_raw(value.handle)) };
            value.handle = ptr::null_mut();
            value.kind = KIND_INVALID;
        }
        KIND_LIST | KIND_TUPLE | KIND_SET | KIND_FROZEN_SET if !value.ptr.is_null() => {
            // SAFETY: recursive raw arrays point to MgRawValue entries.
            let values = unsafe { raw_value_slice_mut(value.ptr, value.len) };
            free_owned_raw_values(values);
        }
        KIND_DICT if !value.ptr.is_null() => {
            // SAFETY: recursive raw arrays point to MgRawPair entries.
            let pairs = unsafe { raw_pair_slice_mut(value.ptr, value.len) };
            for pair in pairs {
                free_owned_raw_handle(&mut pair.key);
                free_owned_raw_handle(&mut pair.value);
            }
        }
        _ => {}
    }
}

pub(crate) fn free_owned_raw_values(values: &mut [MgRawValue]) {
    for value in values {
        free_owned_raw_handle(value);
    }
}

pub(crate) fn read_raw_values(
    ptr: *mut MgRawValue,
    len: usize,
) -> Result<Vec<MontyObject>, MgError> {
    if ptr.is_null() {
        if len == 0 {
            return Ok(Vec::new());
        }
        return Err(ffi_error(
            "TypeError",
            "non-empty raw-value array has null pointer",
        ));
    }
    // SAFETY: caller promises ptr points to len raw values.
    let values = unsafe { slice::from_raw_parts_mut(ptr, len) };
    read_raw_value_slice(values)
}

pub(crate) fn read_raw_values_from_bytes(
    ptr: *mut u8,
    len: usize,
) -> Result<Vec<MontyObject>, MgError> {
    if ptr.is_null() {
        if len == 0 {
            return Ok(Vec::new());
        }
        return Err(ffi_error(
            "TypeError",
            "non-empty raw-value array has null pointer",
        ));
    }
    // SAFETY: raw sequence payloads point to MgRawValue arrays.
    let values = unsafe { raw_value_slice_mut(ptr, len) };
    read_raw_value_slice(values)
}

pub(crate) fn read_raw_value_slice(values: &mut [MgRawValue]) -> Result<Vec<MontyObject>, MgError> {
    let mut objects = Vec::with_capacity(values.len());
    for i in 0..values.len() {
        match read_raw_value(&mut values[i]) {
            Ok(object) => objects.push(object),
            Err(error) => {
                free_owned_raw_values(&mut values[i..]);
                return Err(error);
            }
        }
    }
    Ok(objects)
}

pub(crate) fn text_for_raw(object: &MontyObject) -> std::borrow::Cow<'_, str> {
    use std::borrow::Cow;
    match object {
        MontyObject::String(value)
        | MontyObject::Path(value)
        | MontyObject::Repr(value)
        | MontyObject::Cycle(_, value) => Cow::Borrowed(value),
        MontyObject::BigInt(value) => Cow::Owned(value.to_string()),
        MontyObject::Type(value) => Cow::Owned(value.to_string()),
        MontyObject::BuiltinFunction(value) => Cow::Owned(value.to_string()),
        MontyObject::Function { name, .. } => Cow::Borrowed(name),
        MontyObject::Exception { exc_type, arg } => arg.as_ref().map_or_else(
            || Cow::Owned(exc_type.to_string()),
            |arg| Cow::Owned(format!("{exc_type}: {arg}")),
        ),
        _ => Cow::Owned(object.py_repr()),
    }
}

pub(crate) fn write_raw_value(out: *mut MgRawValue, object: MontyObject) -> Result<(), MgError> {
    if out.is_null() {
        return Err(ffi_error("TypeError", "raw value output pointer is null"));
    }
    let raw = match object {
        MontyObject::Ellipsis => raw_value(KIND_ELLIPSIS),
        MontyObject::None => raw_value(KIND_NONE),
        MontyObject::Bool(value) => {
            let mut raw = raw_value(KIND_BOOL);
            raw.bool_value = u8::from(value);
            raw
        }
        MontyObject::Int(value) => {
            let mut raw = raw_value(KIND_INT);
            raw.int_value = value;
            raw
        }
        MontyObject::Float(value) => {
            let mut raw = raw_value(KIND_FLOAT);
            raw.float_value = value;
            raw
        }
        MontyObject::String(value) => raw_bytes(KIND_STRING, value.as_bytes()),
        MontyObject::BigInt(value) => raw_bytes(KIND_BIG_INT, value.to_string().as_bytes()),
        MontyObject::Bytes(bytes) => raw_bytes(KIND_BYTES, &bytes),
        MontyObject::List(values) => raw_sequence(KIND_LIST, values)?,
        MontyObject::Tuple(values) => raw_sequence(KIND_TUPLE, values)?,
        MontyObject::Set(values) => raw_sequence(KIND_SET, values)?,
        MontyObject::FrozenSet(values) => raw_sequence(KIND_FROZEN_SET, values)?,
        MontyObject::Dict(pairs) => raw_dict(KIND_DICT, pairs)?,
        MontyObject::Path(_)
        | MontyObject::Repr(_)
        | MontyObject::Cycle(_, _)
        | MontyObject::Type(_)
        | MontyObject::BuiltinFunction(_)
        | MontyObject::Function { .. }
        | MontyObject::Exception { .. } => {
            let kind = object_kind(&object);
            raw_bytes(kind, text_for_raw(&object).as_bytes())
        }
        other => {
            let mut raw = raw_value(object_kind(&other));
            raw.handle = Box::into_raw(Box::new(MgValue { object: other }));
            raw
        }
    };
    // SAFETY: out is checked for null above.
    unsafe { *out = raw };
    Ok(())
}

pub(crate) fn write_value_handle(
    out: *mut *mut MgValue,
    object: MontyObject,
) -> Result<(), MgError> {
    if out.is_null() {
        return Err(ffi_error("TypeError", "value output pointer is null"));
    }
    // SAFETY: out is checked for null above.
    unsafe {
        *out = Box::into_raw(Box::new(MgValue { object }));
    }
    Ok(())
}

pub(crate) fn check_raw_output<T>(
    out: *mut T,
    len: usize,
    expected: usize,
    name: &str,
) -> Result<(), MgError> {
    if len != expected {
        return Err(ffi_error(
            "IndexError",
            format!("{name} output length {len} does not match expected {expected}"),
        ));
    }
    if expected != 0 && out.is_null() {
        return Err(ffi_error(
            "TypeError",
            format!("{name} output pointer is null"),
        ));
    }
    Ok(())
}

pub(crate) fn write_raw_values(
    out: *mut MgRawValue,
    len: usize,
    values: &[MontyObject],
    name: &str,
) -> Result<(), MgError> {
    check_raw_output(out, len, values.len(), name)?;
    for (i, value) in values.iter().enumerate() {
        // SAFETY: out is checked for null above when len is non-zero.
        write_raw_value(unsafe { out.add(i) }, value.clone())?;
    }
    Ok(())
}

pub(crate) fn write_raw_pair(
    out: *mut MgRawPair,
    key: &MontyObject,
    value: &MontyObject,
) -> Result<(), MgError> {
    if out.is_null() {
        return Err(ffi_error("TypeError", "raw pair output pointer is null"));
    }
    // SAFETY: out is checked for null above.
    unsafe {
        write_raw_value(ptr::addr_of_mut!((*out).key), key.clone())?;
        write_raw_value(ptr::addr_of_mut!((*out).value), value.clone())?;
    }
    Ok(())
}

pub(crate) fn write_dict_pairs_raw(
    out: *mut MgRawPair,
    len: usize,
    pairs: &DictPairs,
    name: &str,
) -> Result<(), MgError> {
    check_raw_output(out, len, pairs.len(), name)?;
    for (i, (key, value)) in pairs.into_iter().enumerate() {
        // SAFETY: out is checked for null above when len is non-zero.
        write_raw_pair(unsafe { out.add(i) }, key, value)?;
    }
    Ok(())
}

pub(crate) fn free_owned_raw_pairs(pairs: &mut [MgRawPair]) {
    for pair in pairs {
        free_owned_raw_handle(&mut pair.key);
        free_owned_raw_handle(&mut pair.value);
    }
}

pub(crate) fn read_raw_pairs(
    ptr: *mut MgRawPair,
    len: usize,
) -> Result<Vec<(MontyObject, MontyObject)>, MgError> {
    if ptr.is_null() {
        if len == 0 {
            return Ok(Vec::new());
        }
        return Err(ffi_error(
            "TypeError",
            "non-empty raw pair array has null pointer",
        ));
    }
    // SAFETY: caller promises ptr points to len raw pairs.
    let pairs = unsafe { slice::from_raw_parts_mut(ptr, len) };
    read_raw_pair_slice(pairs)
}

pub(crate) fn read_raw_pairs_from_bytes(
    ptr: *mut u8,
    len: usize,
) -> Result<Vec<(MontyObject, MontyObject)>, MgError> {
    if ptr.is_null() {
        if len == 0 {
            return Ok(Vec::new());
        }
        return Err(ffi_error(
            "TypeError",
            "non-empty raw pair array has null pointer",
        ));
    }
    // SAFETY: raw mapping payloads point to MgRawPair arrays.
    let pairs = unsafe { raw_pair_slice_mut(ptr, len) };
    read_raw_pair_slice(pairs)
}

pub(crate) fn read_raw_pair_slice(
    pairs: &mut [MgRawPair],
) -> Result<Vec<(MontyObject, MontyObject)>, MgError> {
    let mut objects = Vec::with_capacity(pairs.len());
    for i in 0..pairs.len() {
        let key = match read_raw_value(&mut pairs[i].key) {
            Ok(key) => key,
            Err(error) => {
                free_owned_raw_pairs(&mut pairs[i..]);
                return Err(error);
            }
        };
        let value = match read_raw_value(&mut pairs[i].value) {
            Ok(value) => value,
            Err(error) => {
                free_owned_raw_handle(&mut pairs[i].value);
                free_owned_raw_pairs(&mut pairs[i + 1..]);
                return Err(error);
            }
        };
        objects.push((key, value));
    }
    Ok(objects)
}

/// Parses an exception type name plus message. Types Monty does not know
/// (user-defined Go exception names) degrade to `RuntimeError` with the
/// original type folded into the message, so a host error stays catchable
/// inside Python instead of aborting the run.
pub(crate) fn parse_exc_type(type_str: &str, message: &str) -> (ExcType, Option<String>) {
    if let Ok(exc_type) = type_str.parse::<ExcType>() {
        return (exc_type, (!message.is_empty()).then(|| message.to_owned()));
    }
    let message = match (type_str.is_empty(), message.is_empty()) {
        (true, true) => None,
        (true, false) => Some(message.to_owned()),
        (false, true) => Some(type_str.to_owned()),
        (false, false) => Some(format!("{type_str}: {message}")),
    };
    (ExcType::RuntimeError, message)
}

pub(crate) fn exception_from_raw(
    exc_type: MgStr,
    message: MgStr,
) -> Result<MontyException, MgError> {
    let (exc_type, message) = parse_exc_type(as_str(exc_type)?, as_str(message)?);
    Ok(MontyException::new(exc_type, message))
}

pub(crate) fn free_raw_pair(pair: &mut MgRawPair) {
    free_raw_value(&mut pair.key);
    free_raw_value(&mut pair.value);
}

pub(crate) fn free_raw_value(value: &mut MgRawValue) {
    match value.kind {
        KIND_LIST | KIND_TUPLE | KIND_SET | KIND_FROZEN_SET if !value.ptr.is_null() => {
            // SAFETY: recursive raw output arrays are allocated by raw_sequence.
            let mut items = unsafe { raw_value_box_from_raw(value.ptr, value.len) };
            for item in &mut items {
                free_raw_value(item);
            }
        }
        KIND_DICT if !value.ptr.is_null() => {
            // SAFETY: recursive raw output arrays are allocated by raw_dict.
            let mut pairs = unsafe { raw_pair_box_from_raw(value.ptr, value.len) };
            for pair in &mut pairs {
                free_raw_pair(pair);
            }
        }
        _ if !value.ptr.is_null() => {
            // SAFETY: ptr/len must come from raw_bytes, which allocates a boxed slice.
            unsafe {
                drop(Box::from_raw(ptr::slice_from_raw_parts_mut(
                    value.ptr, value.len,
                )));
            };
        }
        _ => {}
    }
    if !value.handle.is_null() {
        // SAFETY: handle must come from write_raw_value, which allocates a boxed MgValue.
        unsafe { drop(Box::from_raw(value.handle)) };
    }
    *value = raw_value(KIND_INVALID);
}

// ---------------------------------------------------------------------------
// Limits
// ---------------------------------------------------------------------------

pub(crate) struct LimitSpec {
    pub(crate) limits: Option<ResourceLimits>,
    pub(crate) cancel: Option<Arc<std::sync::atomic::AtomicBool>>,
}

/// Reads `MgLimits` into a `LimitSpec`. A non-null cancel token forces a
/// tracked (`GoTracker`) execution even when no numeric limits are set.
pub(crate) fn limits_from_raw(raw: *const MgLimits) -> LimitSpec {
    if raw.is_null() {
        return LimitSpec {
            limits: None,
            cancel: None,
        };
    }
    // SAFETY: raw is checked for null above and copied immediately.
    let raw = unsafe { &*raw };
    let cancel = if raw.cancel_token.is_null() {
        None
    } else {
        // SAFETY: cancel token handles are allocated with Box::into_raw in this crate.
        Some(unsafe { (*raw.cancel_token).0.clone() })
    };
    let any_limit = raw.max_allocations_set != 0
        || raw.max_duration_nanos_set != 0
        || raw.max_memory_set != 0
        || raw.gc_interval_set != 0
        || raw.max_recursion_depth_set != 0
        || raw.disable_recursion_limit != 0;
    if !any_limit && cancel.is_none() {
        return LimitSpec {
            limits: None,
            cancel: None,
        };
    }
    let max_recursion_depth = if raw.disable_recursion_limit != 0 {
        None
    } else if raw.max_recursion_depth_set != 0 {
        Some(raw.max_recursion_depth)
    } else {
        Some(monty::DEFAULT_MAX_RECURSION_DEPTH)
    };
    LimitSpec {
        limits: Some(ResourceLimits {
            max_allocations: (raw.max_allocations_set != 0).then_some(raw.max_allocations),
            max_duration: (raw.max_duration_nanos_set != 0)
                .then_some(Duration::from_nanos(raw.max_duration_nanos)),
            max_memory: (raw.max_memory_set != 0).then_some(raw.max_memory),
            gc_interval: (raw.gc_interval_set != 0).then_some(raw.gc_interval),
            max_recursion_depth,
        }),
        cancel,
    }
}

impl LimitSpec {
    /// Whether execution needs the tracked (`GoTracker`) path.
    pub(crate) const fn tracked(&self) -> bool {
        self.limits.is_some() || self.cancel.is_some()
    }

    pub(crate) fn tracker(self) -> GoTracker {
        // ResourceLimits::new (not ::default) keeps the CPython-style default
        // recursion limit when no explicit limits were set.
        fn unrestricted() -> ResourceLimits {
            ResourceLimits::new()
        }
        GoTracker::new(self.limits.unwrap_or_else(unrestricted), self.cancel)
    }
}

// ---------------------------------------------------------------------------
// Shared buffer/handle release entry points
// ---------------------------------------------------------------------------

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_bytes_free(ptr: *mut u8, len: usize) {
    if !ptr.is_null() {
        // SAFETY: ptr/len must come from write_bytes, which allocates a boxed slice.
        unsafe { drop(Box::from_raw(ptr::slice_from_raw_parts_mut(ptr, len))) };
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_raw_value_free(value: *mut MgRawValue) {
    if value.is_null() {
        return;
    }
    // SAFETY: value is checked for null above.
    let value = unsafe { &mut *value };
    free_raw_value(value);
}

#[cfg(test)]
mod layout_tests {
    use super::*;
    use std::mem::{offset_of, size_of};

    /// Layout constants mirrored by `internal/ffi/layout_test.go` on the Go
    /// side. Both suites fail when a struct changes on only one side.
    #[test]
    fn abi_struct_layouts() {
        assert_eq!(size_of::<MgStr>(), 16);
        assert_eq!(size_of::<MgBytes>(), 16);
        assert_eq!(size_of::<MgRawValue>(), 48);
        assert_eq!(offset_of!(MgRawValue, int_value), 8);
        assert_eq!(offset_of!(MgRawValue, float_value), 16);
        assert_eq!(offset_of!(MgRawValue, ptr), 24);
        assert_eq!(offset_of!(MgRawValue, len), 32);
        assert_eq!(offset_of!(MgRawValue, handle), 40);
        assert_eq!(size_of::<MgRawPair>(), 96);
        assert_eq!(size_of::<MgLimits>(), 96);
        assert_eq!(offset_of!(MgLimits, cancel_token), 88);
        assert_eq!(size_of::<MgRunFastOutput>(), 104 + FAST_SCRATCH_CAP);
        assert_eq!(offset_of!(MgRunFastOutput, value), 16);
        assert_eq!(offset_of!(MgRunFastOutput, bytes), 64);
        assert_eq!(offset_of!(MgRunFastOutput, print), 80);
        assert_eq!(offset_of!(MgRunFastOutput, error), 96);
        assert_eq!(offset_of!(MgRunFastOutput, scratch), 104);
        assert_eq!(size_of::<MgProgressSnapshot>(), 16 + 16 + 48 * 3);
        assert_eq!(offset_of!(MgProgressSnapshot, name), 16);
        assert_eq!(
            size_of::<MgProgressSnapshotOutput>(),
            48 + size_of::<MgProgressSnapshot>()
        );
        assert_eq!(offset_of!(MgProgressSnapshotOutput, repl), 8);
        assert_eq!(offset_of!(MgProgressSnapshotOutput, print), 24);
        assert_eq!(offset_of!(MgProgressSnapshotOutput, print_flags), 40);
        assert_eq!(offset_of!(MgProgressSnapshotOutput, snapshot), 48);
        assert_eq!(size_of::<MgRunJsonOutput>(), 48);
        assert_eq!(size_of::<MgHostFunctionOutput>(), 48 + 32);
        assert_eq!(size_of::<MgFutureResult>(), 8 + 48 + 32);
        assert_eq!(size_of::<MgRunHostArgs>(), 88);
        assert_eq!(size_of::<MgReplFeedArgs>(), 128);
        assert_eq!(offset_of!(MgReplFeedArgs, max_duration_nanos), 48);
        assert_eq!(offset_of!(MgReplFeedArgs, host_names), 64);
        assert_eq!(size_of::<MgMountOutput>(), 64);
        assert_eq!(size_of::<MgDate>(), 8);
        assert_eq!(size_of::<MgDateTime>(), 36 + 4);
        assert_eq!(size_of::<MgTimeDelta>(), 12);
        assert_eq!(size_of::<MgTimeZone>(), 24);
    }
}
