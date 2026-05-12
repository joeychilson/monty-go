#![allow(clippy::missing_safety_doc)]

use std::{
    panic::{AssertUnwindSafe, catch_unwind},
    ptr, slice, str,
    sync::{Arc, Mutex},
    time::Duration,
};

use monty::fs::{Mount as MontyMount, MountMode as MontyMountMode, MountTable};
use monty::{
    DictPairs, ExcType, ExtFunctionResult, FunctionCall, JsonMontyObject, LimitedTracker,
    MontyDate, MontyDateTime, MontyException, MontyObject, MontyRepl, MontyRun, MontyTimeDelta,
    MontyTimeZone, NameLookup, NameLookupResult, NoLimitTracker, OsCall, OsFunction, PrintWriter,
    ReplContinuationMode, ResourceLimits, ResourceTracker, RunProgress,
    detect_repl_continuation_mode,
};
use monty_type_checking::{SourceFile, type_check};
use num_bigint::BigInt;
use serde::{Deserialize, Serialize};

const STATUS_OK: i32 = 0;
const STATUS_ERR: i32 = 1;

const FAST_FORMAT_RAW: u32 = 0;
const FAST_FORMAT_FLAT: u32 = 1;

const PROGRESS_FUNCTION_CALL: u32 = 1;
const PROGRESS_OS_CALL: u32 = 2;
const PROGRESS_RESOLVE_FUTURES: u32 = 3;
const PROGRESS_NAME_LOOKUP: u32 = 4;
const PROGRESS_COMPLETE: u32 = 5;

const FUTURE_RESULT_RETURN: u32 = 0;
const FUTURE_RESULT_ERROR: u32 = 1;
const FUTURE_RESULT_NOT_FOUND: u32 = 2;

const HOST_CALLBACK_RETURN: i32 = 0;
const HOST_CALLBACK_EXCEPTION: i32 = 1;

const KIND_INVALID: u32 = 0;
const KIND_ELLIPSIS: u32 = 1;
const KIND_NONE: u32 = 2;
const KIND_BOOL: u32 = 3;
const KIND_INT: u32 = 4;
const KIND_BIG_INT: u32 = 5;
const KIND_FLOAT: u32 = 6;
const KIND_STRING: u32 = 7;
const KIND_BYTES: u32 = 8;
const KIND_LIST: u32 = 9;
const KIND_TUPLE: u32 = 10;
const KIND_NAMED_TUPLE: u32 = 11;
const KIND_DICT: u32 = 12;
const KIND_SET: u32 = 13;
const KIND_FROZEN_SET: u32 = 14;
const KIND_DATE: u32 = 15;
const KIND_DATETIME: u32 = 16;
const KIND_TIME_DELTA: u32 = 17;
const KIND_TIME_ZONE: u32 = 18;
const KIND_EXCEPTION: u32 = 19;
const KIND_TYPE: u32 = 20;
const KIND_BUILTIN_FUNCTION: u32 = 21;
const KIND_PATH: u32 = 22;
const KIND_DATACLASS: u32 = 23;
const KIND_FUNCTION: u32 = 24;
const KIND_REPR: u32 = 25;
const KIND_CYCLE: u32 = 26;
const KIND_OWNED_HANDLE: u32 = u32::MAX;

#[repr(C)]
#[derive(Clone, Copy)]
pub struct MgStr {
    ptr: *const u8,
    len: usize,
}

#[repr(C)]
pub struct MgBytes {
    ptr: *mut u8,
    len: usize,
}

#[repr(C)]
pub struct MgLimits {
    max_allocations_set: u8,
    max_allocations: usize,
    max_duration_nanos_set: u8,
    max_duration_nanos: u64,
    max_memory_set: u8,
    max_memory: usize,
    gc_interval_set: u8,
    gc_interval: usize,
    max_recursion_depth_set: u8,
    max_recursion_depth: usize,
    disable_recursion_limit: u8,
}

#[repr(C)]
pub struct MgProgramCompileArgs {
    code: MgStr,
    script_name: MgStr,
    input_names: *const MgStr,
    input_count: usize,
}

#[repr(C)]
pub struct MgCompileRunFastRawArgs {
    code: MgStr,
    script_name: MgStr,
    input_names: *const MgStr,
    input_count: usize,
    input_values: *mut MgRawValue,
    input_value_count: usize,
    limits: *const MgLimits,
}

#[repr(C)]
pub struct MgMountNewArgs {
    virtual_path: MgStr,
    host_path: MgStr,
    mode: u32,
    has_write_bytes_limit: u8,
    _pad: [u8; 3],
    write_bytes_limit: u64,
}

#[repr(C)]
pub struct MgMountCallArgs {
    mounts: *const *mut MgMount,
    mount_count: usize,
    function: MgStr,
    args: *const *const MgValue,
    arg_count: usize,
    kwarg_keys: *const *const MgValue,
    kwarg_values: *const *const MgValue,
    kwarg_count: usize,
}

#[repr(C)]
pub struct MgReplNewArgs {
    script_name: MgStr,
    limits: *const MgLimits,
}

#[repr(C)]
pub struct MgReplFeedRunArgs {
    code: MgStr,
    input_names: *const MgStr,
    input_values: *const *const MgValue,
    input_count: usize,
}

#[repr(C)]
pub struct MgReplCallArgs {
    name: MgStr,
    args: *const *const MgValue,
    arg_count: usize,
}

#[repr(C)]
pub struct MgFutureResult {
    call_id: u32,
    kind: u32,
    value: MgRawValue,
    exc_type: MgStr,
    message: MgStr,
}

#[repr(C)]
pub struct MgRawValue {
    kind: u32,
    bool_value: u8,
    _pad: [u8; 3],
    int_value: i64,
    float_value: f64,
    ptr: *mut u8,
    len: usize,
    handle: *mut MgValue,
}

#[repr(C)]
pub struct MgRunOutput {
    value: *mut MgValue,
    print: MgBytes,
    error: *mut MgError,
}

#[repr(C)]
pub struct MgStartOutput {
    progress: *mut MgProgress,
    print: MgBytes,
    error: *mut MgError,
}

#[repr(C)]
pub struct MgRunRawOutput {
    value: MgRawValue,
    print: MgBytes,
    error: *mut MgError,
}

#[repr(C)]
pub struct MgRunJsonOutput {
    value: MgBytes,
    print: MgBytes,
    error: *mut MgError,
}

/// Inline scratch capacity used for flat-format result bytes. Sized to cover
/// every value produced by the benchmark suite (records-of-100 lands around
/// 3 KiB) so the Go caller never needs a second cgocall to free a heap buffer.
const FAST_SCRATCH_CAP: usize = 8192;

#[repr(C)]
pub struct MgRunFastOutput {
    format: u32,
    /// Set to 1 when `bytes.ptr` points into `scratch` (Go-owned, no free
    /// required); 0 when `bytes` points at a Rust-owned heap allocation that
    /// must be released with `mg_bytes_free`.
    bytes_in_scratch: u32,
    value: MgRawValue,
    bytes: MgBytes,
    print: MgBytes,
    error: *mut MgError,
    /// Caller-owned scratch buffer. Filled by the Rust side when a flat-encoded
    /// result fits; otherwise the Rust side allocates `bytes` separately and
    /// leaves `scratch` untouched.
    scratch: [u8; FAST_SCRATCH_CAP],
}

#[repr(C)]
pub struct MgProgressOutput {
    progress: *mut MgProgress,
    print: MgBytes,
    error: *mut MgError,
}

#[repr(C)]
pub struct MgProgressSnapshot {
    kind: u32,
    name: MgBytes,
    args: MgRawValue,
    kwargs: MgRawValue,
    value: MgRawValue,
    call_id: u32,
    method_call: u8,
    _pad: [u8; 3],
    error: *mut MgError,
}

#[repr(C)]
pub struct MgProgressSnapshotOutput {
    progress: *mut MgProgress,
    snapshot: MgProgressSnapshot,
    print: MgBytes,
    error: *mut MgError,
}

#[repr(C)]
pub struct MgHostFunctionOutput {
    value: MgRawValue,
    exc_type: MgStr,
    message: MgStr,
}

type MgHostFunctionCallback = Option<
    unsafe extern "C" fn(
        user_data: usize,
        name_ptr: *const u8,
        name_len: usize,
        args: *const MgRawValue,
        kwargs: *const MgRawValue,
        out: *mut MgHostFunctionOutput,
    ) -> i32,
>;

#[repr(C)]
pub struct MgMountOutput {
    value: *mut MgValue,
    error: *mut MgError,
    handled: u8,
}

#[repr(C)]
pub struct MgRawPair {
    key: MgRawValue,
    value: MgRawValue,
}

#[repr(C)]
#[derive(Clone, Copy)]
pub struct MgDate {
    year: i32,
    month: u8,
    day: u8,
    _pad: [u8; 2],
}

#[repr(C)]
pub struct MgDateTime {
    timezone_name: MgBytes,
    year: i32,
    microsecond: u32,
    offset_seconds: i32,
    month: u8,
    day: u8,
    hour: u8,
    minute: u8,
    second: u8,
    has_offset: u8,
    has_timezone_name: u8,
    _pad: u8,
}

#[repr(C)]
#[derive(Clone, Copy)]
pub struct MgTimeDelta {
    days: i32,
    seconds: i32,
    microseconds: i32,
}

#[repr(C)]
pub struct MgTimeZone {
    name: MgBytes,
    offset_seconds: i32,
    has_name: u8,
    _pad: [u8; 3],
}

#[repr(C)]
pub struct MgDataclassRawArgs {
    name: MgStr,
    type_id: u64,
    field_names: *const MgStr,
    field_count: usize,
    attrs: *mut MgRawPair,
    attr_count: usize,
    frozen: u8,
    _pad: [u8; 7],
}

pub struct MgProgram {
    runner: MontyRun,
    script_name: String,
    input_names: Vec<String>,
}

pub struct MgValue {
    object: MontyObject,
}

pub struct MgError {
    exc_type: String,
    message: String,
    display: String,
}

pub struct MgMount {
    slot: Arc<Mutex<Option<MontyMount>>>,
}

#[derive(Serialize, Deserialize)]
pub enum MgProgress {
    NoLimit(RunProgress<NoLimitTracker>),
    Limited(RunProgress<LimitedTracker>),
}

#[derive(Serialize, Deserialize)]
pub enum MgRepl {
    NoLimit(MontyRepl<NoLimitTracker>),
    Limited(MontyRepl<LimitedTracker>),
}

#[derive(Serialize, Deserialize)]
struct StoredProgram {
    runner: MontyRun,
    script_name: String,
    input_names: Vec<String>,
}

fn ffi_error(exc_type: impl Into<String>, message: impl Into<String>) -> MgError {
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
    }
}

fn from_monty_error(exc: &MontyException) -> MgError {
    MgError {
        exc_type: exc.exc_type().to_string(),
        message: exc.message().unwrap_or_default().to_owned(),
        display: exc.to_string(),
    }
}

fn set_error(out_error: *mut *mut MgError, error: MgError) {
    if !out_error.is_null() {
        // SAFETY: out_error is provided by the caller and checked for null above.
        unsafe { *out_error = Box::into_raw(Box::new(error)) };
    }
}

fn guard(out_error: *mut *mut MgError, f: impl FnOnce() -> Result<(), MgError>) -> i32 {
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

fn progress_output_error(out: *mut MgProgressOutput) -> *mut *mut MgError {
    if out.is_null() {
        return ptr::null_mut();
    }
    // SAFETY: out is non-null and points to a caller-provided output struct.
    unsafe { ptr::addr_of_mut!((*out).error) }
}

fn write_progress_output(
    out: *mut MgProgressOutput,
    progress: MgProgress,
    stdout: String,
) -> Result<(), MgError> {
    if out.is_null() {
        return Err(ffi_error("TypeError", "progress output pointer is null"));
    }
    // SAFETY: out is checked for null above.
    unsafe {
        write_owned_string(ptr::addr_of_mut!((*out).print), stdout)?;
        (*out).progress = Box::into_raw(Box::new(progress));
    }
    Ok(())
}

fn as_str(input: MgStr) -> Result<&'static str, MgError> {
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

fn str_arg(ptr: *const u8, len: usize) -> Result<&'static str, MgError> {
    as_str(MgStr { ptr, len })
}

fn as_bytes<'a>(ptr: *const u8, len: usize) -> Result<&'a [u8], MgError> {
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

fn write_bytes(out: *mut MgBytes, bytes: &[u8]) -> Result<(), MgError> {
    if out.is_null() {
        return Err(ffi_error("TypeError", "output bytes pointer is null"));
    }
    if bytes.is_empty() {
        // SAFETY: out is checked for null above and receives an empty buffer.
        unsafe {
            *out = MgBytes {
                ptr: ptr::null_mut(),
                len: 0,
            }
        };
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

fn write_owned_bytes(out: *mut MgBytes, mut bytes: Vec<u8>) -> Result<(), MgError> {
    if out.is_null() {
        return Err(ffi_error("TypeError", "output bytes pointer is null"));
    }
    if bytes.is_empty() {
        // SAFETY: out is checked for null above and receives an empty buffer.
        unsafe {
            *out = MgBytes {
                ptr: ptr::null_mut(),
                len: 0,
            }
        };
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

fn write_string(out: *mut MgBytes, value: &str) -> Result<(), MgError> {
    write_bytes(out, value.as_bytes())
}

fn write_owned_string(out: *mut MgBytes, value: String) -> Result<(), MgError> {
    write_owned_bytes(out, value.into_bytes())
}

fn read_value(value: *const MgValue) -> Result<MontyObject, MgError> {
    if value.is_null() {
        return Err(ffi_error("TypeError", "value handle is null"));
    }
    // SAFETY: handle validity is owned by the Go side contract.
    Ok(unsafe { (*value).object.clone() })
}

fn read_values(ptr: *const *const MgValue, len: usize) -> Result<Vec<MontyObject>, MgError> {
    if ptr.is_null() {
        if len == 0 {
            return Ok(Vec::new());
        }
        return Err(ffi_error(
            "TypeError",
            "non-empty value array has null pointer",
        ));
    }
    // SAFETY: caller promises ptr points to len value handles.
    let handles = unsafe { slice::from_raw_parts(ptr, len) };
    handles.iter().map(|handle| read_value(*handle)).collect()
}

fn read_value_pairs(
    keys: *const *const MgValue,
    values: *const *const MgValue,
    len: usize,
) -> Result<Vec<(MontyObject, MontyObject)>, MgError> {
    if keys.is_null() || values.is_null() {
        if len == 0 {
            return Ok(Vec::new());
        }
        return Err(ffi_error(
            "TypeError",
            "non-empty value pair array has null pointer",
        ));
    }
    // SAFETY: caller promises keys and values point to len value handles.
    let keys = unsafe { slice::from_raw_parts(keys, len) };
    // SAFETY: caller promises keys and values point to len value handles.
    let values = unsafe { slice::from_raw_parts(values, len) };
    keys.iter()
        .zip(values)
        .map(|(key, value)| Ok((read_value(*key)?, read_value(*value)?)))
        .collect()
}

fn read_named_values(
    names: *const MgStr,
    values: *const *const MgValue,
    len: usize,
) -> Result<Vec<(String, MontyObject)>, MgError> {
    let names = read_string_list(names, len)?;
    let values = read_values(values, len)?;
    Ok(names.into_iter().zip(values).collect())
}

fn read_string_list(ptr: *const MgStr, len: usize) -> Result<Vec<String>, MgError> {
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

fn string_list_value(values: &[String]) -> *mut MgValue {
    let items = values
        .iter()
        .map(|value| MontyObject::String(value.clone()))
        .collect();
    Box::into_raw(Box::new(MgValue {
        object: MontyObject::List(items),
    }))
}

const fn monty_date_from_raw(raw: MgDate) -> MontyDate {
    MontyDate {
        year: raw.year,
        month: raw.month,
        day: raw.day,
    }
}

fn monty_datetime_from_raw(raw: &MgDateTime) -> Result<MontyDateTime, MgError> {
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

const fn monty_timedelta_from_raw(raw: MgTimeDelta) -> MontyTimeDelta {
    MontyTimeDelta {
        days: raw.days,
        seconds: raw.seconds,
        microseconds: raw.microseconds,
    }
}

fn monty_timezone_from_raw(raw: &MgTimeZone) -> Result<MontyTimeZone, MgError> {
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

const fn raw_value(kind: u32) -> MgRawValue {
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

fn raw_bytes(kind: u32, bytes: &[u8]) -> MgRawValue {
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
const unsafe fn raw_value_slice_mut<'a>(ptr: *mut u8, len: usize) -> &'a mut [MgRawValue] {
    // SAFETY: callers only pass pointers that were originally allocated as
    // MgRawValue arrays by this crate or received from Go's matching ABI type.
    unsafe { slice::from_raw_parts_mut(ptr.cast::<MgRawValue>(), len) }
}

#[allow(clippy::cast_ptr_alignment)]
const unsafe fn raw_pair_slice_mut<'a>(ptr: *mut u8, len: usize) -> &'a mut [MgRawPair] {
    // SAFETY: callers only pass pointers that were originally allocated as
    // MgRawPair arrays by this crate or received from Go's matching ABI type.
    unsafe { slice::from_raw_parts_mut(ptr.cast::<MgRawPair>(), len) }
}

#[allow(clippy::cast_ptr_alignment)]
unsafe fn raw_value_box_from_raw(ptr: *mut u8, len: usize) -> Box<[MgRawValue]> {
    // SAFETY: ptr/len must come from raw_sequence, which allocated a boxed
    // MgRawValue slice and stored its data pointer in MgRawValue.ptr.
    unsafe { Box::from_raw(ptr::slice_from_raw_parts_mut(ptr.cast::<MgRawValue>(), len)) }
}

#[allow(clippy::cast_ptr_alignment)]
unsafe fn raw_pair_box_from_raw(ptr: *mut u8, len: usize) -> Box<[MgRawPair]> {
    // SAFETY: ptr/len must come from raw_dict, which allocated a boxed
    // MgRawPair slice and stored its data pointer in MgRawValue.ptr.
    unsafe { Box::from_raw(ptr::slice_from_raw_parts_mut(ptr.cast::<MgRawPair>(), len)) }
}

fn raw_sequence(kind: u32, values: Vec<MontyObject>) -> Result<MgRawValue, MgError> {
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

fn raw_dict(kind: u32, pairs: DictPairs) -> Result<MgRawValue, MgError> {
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

fn push_flat_u8(out: &mut Vec<u8>, value: u8) {
    out.push(value);
}

fn push_flat_u32(out: &mut Vec<u8>, value: usize) -> Result<(), MgError> {
    let value = u32::try_from(value)
        .map_err(|_| ffi_error("OverflowError", "flat value length exceeds u32"))?;
    out.extend_from_slice(&value.to_le_bytes());
    Ok(())
}

fn push_flat_i64(out: &mut Vec<u8>, value: i64) {
    out.extend_from_slice(&value.to_le_bytes());
}

fn push_flat_f64(out: &mut Vec<u8>, value: f64) {
    out.extend_from_slice(&value.to_le_bytes());
}

fn push_flat_bytes(out: &mut Vec<u8>, bytes: &[u8]) -> Result<(), MgError> {
    push_flat_u32(out, bytes.len())?;
    out.extend_from_slice(bytes);
    Ok(())
}

fn write_flat_value(out: &mut Vec<u8>, object: &MontyObject) -> Result<(), MgError> {
    out.extend_from_slice(&object_kind(object).to_le_bytes());
    match object {
        MontyObject::Ellipsis | MontyObject::None => Ok(()),
        MontyObject::Bool(value) => {
            push_flat_u8(out, u8::from(*value));
            Ok(())
        }
        MontyObject::Int(value) => {
            push_flat_i64(out, *value);
            Ok(())
        }
        MontyObject::BigInt(value) => push_flat_bytes(out, value.to_string().as_bytes()),
        MontyObject::Float(value) => {
            push_flat_f64(out, *value);
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

const fn object_kind(object: &MontyObject) -> u32 {
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

fn read_raw_value(value: &mut MgRawValue) -> Result<MontyObject, MgError> {
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

fn take_owned_raw_handle(value: &mut MgRawValue) -> Result<MontyObject, MgError> {
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

fn free_owned_raw_handle(value: &mut MgRawValue) {
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

fn free_owned_raw_values(values: &mut [MgRawValue]) {
    for value in values {
        free_owned_raw_handle(value);
    }
}

fn read_raw_values(ptr: *mut MgRawValue, len: usize) -> Result<Vec<MontyObject>, MgError> {
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

fn read_raw_values_from_bytes(ptr: *mut u8, len: usize) -> Result<Vec<MontyObject>, MgError> {
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

fn read_raw_value_slice(values: &mut [MgRawValue]) -> Result<Vec<MontyObject>, MgError> {
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

fn text_for_raw(object: &MontyObject) -> String {
    match object {
        MontyObject::String(value)
        | MontyObject::Path(value)
        | MontyObject::Repr(value)
        | MontyObject::Cycle(_, value) => value.clone(),
        MontyObject::BigInt(value) => value.to_string(),
        MontyObject::Type(value) => value.to_string(),
        MontyObject::BuiltinFunction(value) => value.to_string(),
        MontyObject::Function { name, .. } => name.clone(),
        MontyObject::Exception { exc_type, arg } => arg
            .as_ref()
            .map_or_else(|| exc_type.to_string(), |arg| format!("{exc_type}: {arg}")),
        _ => object.py_repr(),
    }
}

fn write_raw_value(out: *mut MgRawValue, object: MontyObject) -> Result<(), MgError> {
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

fn write_value_handle(out: *mut *mut MgValue, object: MontyObject) -> Result<(), MgError> {
    if out.is_null() {
        return Err(ffi_error("TypeError", "value output pointer is null"));
    }
    // SAFETY: out is checked for null above.
    unsafe {
        *out = Box::into_raw(Box::new(MgValue { object }));
    }
    Ok(())
}

fn check_raw_output<T>(
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

fn write_raw_values(
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

fn write_raw_pair(
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

fn write_dict_pairs_raw(
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

fn free_owned_raw_pairs(pairs: &mut [MgRawPair]) {
    for pair in pairs {
        free_owned_raw_handle(&mut pair.key);
        free_owned_raw_handle(&mut pair.value);
    }
}

fn read_raw_pairs(
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

fn read_raw_pairs_from_bytes(
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

fn read_raw_pair_slice(
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

fn exception_from_raw(exc_type: MgStr, message: MgStr) -> Result<MontyException, MgError> {
    let exc_type = as_str(exc_type)?
        .parse::<ExcType>()
        .map_err(|_| ffi_error("ValueError", "unknown exception type"))?;
    let message = as_str(message)?;
    Ok(MontyException::new(
        exc_type,
        (!message.is_empty()).then(|| message.to_owned()),
    ))
}

fn read_future_results(
    ptr: *mut MgFutureResult,
    len: usize,
) -> Result<Vec<(u32, ExtFunctionResult)>, MgError> {
    if ptr.is_null() {
        if len == 0 {
            return Ok(Vec::new());
        }
        return Err(ffi_error(
            "TypeError",
            "non-empty future-result array has null pointer",
        ));
    }
    // SAFETY: caller promises ptr points to len future result entries.
    let results = unsafe { slice::from_raw_parts_mut(ptr, len) };
    let mut values = Vec::with_capacity(len);
    for i in 0..len {
        let value = match results[i].kind {
            FUTURE_RESULT_RETURN => match read_raw_value(&mut results[i].value) {
                Ok(value) => ExtFunctionResult::Return(value),
                Err(error) => {
                    free_owned_raw_handle(&mut results[i].value);
                    for result in &mut results[i + 1..] {
                        free_owned_raw_handle(&mut result.value);
                    }
                    return Err(error);
                }
            },
            FUTURE_RESULT_ERROR => {
                match exception_from_raw(results[i].exc_type, results[i].message) {
                    Ok(error) => ExtFunctionResult::Error(error),
                    Err(error) => {
                        for result in &mut results[i + 1..] {
                            free_owned_raw_handle(&mut result.value);
                        }
                        return Err(error);
                    }
                }
            }
            FUTURE_RESULT_NOT_FOUND => match as_str(results[i].message) {
                Ok(name) => ExtFunctionResult::NotFound(name.to_owned()),
                Err(error) => {
                    for result in &mut results[i + 1..] {
                        free_owned_raw_handle(&mut result.value);
                    }
                    return Err(error);
                }
            },
            _ => {
                free_owned_raw_handle(&mut results[i].value);
                for result in &mut results[i + 1..] {
                    free_owned_raw_handle(&mut result.value);
                }
                return Err(ffi_error(
                    "ValueError",
                    format!("unknown future result kind {}", results[i].kind),
                ));
            }
        };
        values.push((results[i].call_id, value));
    }
    Ok(values)
}

fn limits_from_raw(raw: *const MgLimits) -> Option<ResourceLimits> {
    if raw.is_null() {
        return None;
    }
    // SAFETY: raw is checked for null above and copied immediately.
    let raw = unsafe { &*raw };
    let max_recursion_depth = if raw.disable_recursion_limit != 0 {
        None
    } else if raw.max_recursion_depth_set != 0 {
        Some(raw.max_recursion_depth)
    } else {
        Some(monty::DEFAULT_MAX_RECURSION_DEPTH)
    };

    Some(ResourceLimits {
        max_allocations: (raw.max_allocations_set != 0).then_some(raw.max_allocations),
        max_duration: (raw.max_duration_nanos_set != 0)
            .then_some(Duration::from_nanos(raw.max_duration_nanos)),
        max_memory: (raw.max_memory_set != 0).then_some(raw.max_memory),
        gc_interval: (raw.gc_interval_set != 0).then_some(raw.gc_interval),
        max_recursion_depth,
    })
}

fn start_with_print(
    program: &MgProgram,
    inputs: Vec<MontyObject>,
    limits: Option<ResourceLimits>,
) -> Result<(MgProgress, String), MgError> {
    let mut stdout = String::new();
    let writer = PrintWriter::CollectString(&mut stdout);
    let progress = if let Some(limits) = limits {
        program
            .runner
            .clone()
            .start(inputs, LimitedTracker::new(limits), writer)
            .map(MgProgress::Limited)
    } else {
        program
            .runner
            .clone()
            .start(inputs, NoLimitTracker, writer)
            .map(MgProgress::NoLimit)
    }
    .map_err(|exc| from_monty_error(&exc))?;
    Ok((progress, stdout))
}

fn run_with_print(
    program: &MgProgram,
    inputs: Vec<MontyObject>,
    limits: Option<ResourceLimits>,
) -> Result<(MontyObject, String), MgError> {
    let mut stdout = String::new();
    let writer = PrintWriter::CollectString(&mut stdout);
    let value = if let Some(limits) = limits {
        program
            .runner
            .run(inputs, LimitedTracker::new(limits), writer)
    } else {
        program.runner.run(inputs, NoLimitTracker, writer)
    }
    .map_err(|exc| from_monty_error(&exc))?;
    Ok((value, stdout))
}

fn run_with_host_callback(
    program: &MgProgram,
    inputs: Vec<MontyObject>,
    limits: Option<ResourceLimits>,
    host_names: &[String],
    callback: MgHostFunctionCallback,
    user_data: usize,
) -> Result<(MontyObject, String), MgError> {
    let callback =
        callback.ok_or_else(|| ffi_error("TypeError", "host function callback is null"))?;
    let (progress, mut stdout) = start_with_print(program, inputs, limits)?;
    let value = match progress {
        MgProgress::NoLimit(progress) => {
            run_host_progress_loop(progress, host_names, callback, user_data, &mut stdout)?
        }
        MgProgress::Limited(progress) => {
            run_host_progress_loop(progress, host_names, callback, user_data, &mut stdout)?
        }
    };
    Ok((value, stdout))
}

fn run_host_progress_loop<T: ResourceTracker>(
    mut progress: RunProgress<T>,
    host_names: &[String],
    callback: unsafe extern "C" fn(
        usize,
        *const u8,
        usize,
        *const MgRawValue,
        *const MgRawValue,
        *mut MgHostFunctionOutput,
    ) -> i32,
    user_data: usize,
    stdout: &mut String,
) -> Result<MontyObject, MgError> {
    loop {
        progress = match progress {
            RunProgress::Complete(value) => return Ok(value),
            RunProgress::NameLookup(lookup) => {
                let result = if host_names.iter().any(|name| name == &lookup.name) {
                    NameLookupResult::Value(MontyObject::Function {
                        name: lookup.name.clone(),
                        docstring: None,
                    })
                } else {
                    NameLookupResult::Undefined
                };
                let writer = PrintWriter::CollectString(stdout);
                lookup
                    .resume(result, writer)
                    .map_err(|exc| from_monty_error(&exc))?
            }
            RunProgress::FunctionCall(call) => {
                let result = call_host_function_callback(&call, callback, user_data)?;
                let writer = PrintWriter::CollectString(stdout);
                call.resume(result, writer)
                    .map_err(|exc| from_monty_error(&exc))?
            }
            RunProgress::OsCall(_) => {
                return Err(ffi_error(
                    "RuntimeError",
                    "host callback run path does not support OS calls",
                ));
            }
            RunProgress::ResolveFutures(_) => {
                return Err(ffi_error(
                    "RuntimeError",
                    "host callback run path does not support pending futures",
                ));
            }
        };
    }
}

fn call_host_function_callback<T: ResourceTracker>(
    call: &FunctionCall<T>,
    callback: unsafe extern "C" fn(
        usize,
        *const u8,
        usize,
        *const MgRawValue,
        *const MgRawValue,
        *mut MgHostFunctionOutput,
    ) -> i32,
    user_data: usize,
) -> Result<ExtFunctionResult, MgError> {
    if call.args.len() == 1
        && call.kwargs.is_empty()
        && let Some(mut arg) = borrowed_raw_value(&call.args[0])
    {
        let mut args = raw_value(KIND_LIST);
        args.ptr = ptr::addr_of_mut!(arg).cast();
        args.len = 1;
        let kwargs = raw_value(KIND_DICT);
        return invoke_host_function_callback(call, callback, user_data, &args, &kwargs);
    }
    if call.args.is_empty()
        && call.kwargs.len() == 2
        && let (Some(first), Some(second)) = (
            borrowed_raw_pair(&call.kwargs[0]),
            borrowed_raw_pair(&call.kwargs[1]),
        )
    {
        let args = raw_value(KIND_LIST);
        let mut pairs = [first, second];
        let mut kwargs = raw_value(KIND_DICT);
        kwargs.ptr = pairs.as_mut_ptr().cast();
        kwargs.len = pairs.len();
        return invoke_host_function_callback(call, callback, user_data, &args, &kwargs);
    }
    let mut args = raw_sequence(KIND_LIST, call.args.clone())?;
    let mut kwargs = raw_dict(KIND_DICT, call.kwargs.clone().into())?;
    let result = invoke_host_function_callback(call, callback, user_data, &args, &kwargs);
    free_raw_value(&mut args);
    free_raw_value(&mut kwargs);
    result
}

fn invoke_host_function_callback<T: ResourceTracker>(
    call: &FunctionCall<T>,
    callback: unsafe extern "C" fn(
        usize,
        *const u8,
        usize,
        *const MgRawValue,
        *const MgRawValue,
        *mut MgHostFunctionOutput,
    ) -> i32,
    user_data: usize,
    args: &MgRawValue,
    kwargs: &MgRawValue,
) -> Result<ExtFunctionResult, MgError> {
    let mut out = MgHostFunctionOutput {
        value: raw_value(KIND_INVALID),
        exc_type: MgStr {
            ptr: ptr::null(),
            len: 0,
        },
        message: MgStr {
            ptr: ptr::null(),
            len: 0,
        },
    };
    // SAFETY: callback is provided by Go for this FFI call, and all pointers
    // passed here reference stack values or call-owned Monty strings.
    let status = unsafe {
        callback(
            user_data,
            call.function_name.as_ptr(),
            call.function_name.len(),
            ptr::from_ref(args),
            ptr::from_ref(kwargs),
            ptr::addr_of_mut!(out),
        )
    };
    match status {
        HOST_CALLBACK_RETURN => read_raw_value(&mut out.value).map(ExtFunctionResult::Return),
        HOST_CALLBACK_EXCEPTION => {
            exception_from_raw(out.exc_type, out.message).map(ExtFunctionResult::Error)
        }
        _ => Err(ffi_error("RuntimeError", "host function callback failed")),
    }
}

fn borrowed_raw_value(object: &MontyObject) -> Option<MgRawValue> {
    match object {
        MontyObject::Ellipsis => Some(raw_value(KIND_ELLIPSIS)),
        MontyObject::None => Some(raw_value(KIND_NONE)),
        MontyObject::Bool(value) => {
            let mut raw = raw_value(KIND_BOOL);
            raw.bool_value = u8::from(*value);
            Some(raw)
        }
        MontyObject::Int(value) => {
            let mut raw = raw_value(KIND_INT);
            raw.int_value = *value;
            Some(raw)
        }
        MontyObject::Float(value) => {
            let mut raw = raw_value(KIND_FLOAT);
            raw.float_value = *value;
            Some(raw)
        }
        MontyObject::String(value) => {
            let mut raw = raw_value(KIND_STRING);
            if !value.is_empty() {
                raw.ptr = value.as_ptr().cast_mut();
                raw.len = value.len();
            }
            Some(raw)
        }
        MontyObject::Bytes(value) => {
            let mut raw = raw_value(KIND_BYTES);
            if !value.is_empty() {
                raw.ptr = value.as_ptr().cast_mut();
                raw.len = value.len();
            }
            Some(raw)
        }
        _ => None,
    }
}

fn borrowed_raw_pair(pair: &(MontyObject, MontyObject)) -> Option<MgRawPair> {
    Some(MgRawPair {
        key: borrowed_raw_value(&pair.0)?,
        value: borrowed_raw_value(&pair.1)?,
    })
}

fn resume_progress(
    progress: MgProgress,
    result: ExtFunctionResult,
) -> Result<(MgProgress, String), MgError> {
    let mut stdout = String::new();
    let writer = PrintWriter::CollectString(&mut stdout);
    let next = match progress {
        MgProgress::NoLimit(progress) => match progress {
            RunProgress::FunctionCall(call) => call.resume(result, writer),
            RunProgress::OsCall(call) => call.resume(result, writer),
            _ => {
                return Err(ffi_error(
                    "RuntimeError",
                    "progress state cannot be resumed with an external result",
                ));
            }
        }
        .map(MgProgress::NoLimit),
        MgProgress::Limited(progress) => match progress {
            RunProgress::FunctionCall(call) => call.resume(result, writer),
            RunProgress::OsCall(call) => call.resume(result, writer),
            _ => {
                return Err(ffi_error(
                    "RuntimeError",
                    "progress state cannot be resumed with an external result",
                ));
            }
        }
        .map(MgProgress::Limited),
    }
    .map_err(|exc| from_monty_error(&exc))?;
    Ok((next, stdout))
}

fn resume_pending(progress: MgProgress) -> Result<(MgProgress, String), MgError> {
    let mut stdout = String::new();
    let writer = PrintWriter::CollectString(&mut stdout);
    let next = match progress {
        MgProgress::NoLimit(progress) => match progress {
            RunProgress::FunctionCall(call) => call.resume_pending(writer),
            _ => {
                return Err(ffi_error(
                    "RuntimeError",
                    "progress state is not a function call",
                ));
            }
        }
        .map(MgProgress::NoLimit),
        MgProgress::Limited(progress) => match progress {
            RunProgress::FunctionCall(call) => call.resume_pending(writer),
            _ => {
                return Err(ffi_error(
                    "RuntimeError",
                    "progress state is not a function call",
                ));
            }
        }
        .map(MgProgress::Limited),
    }
    .map_err(|exc| from_monty_error(&exc))?;
    Ok((next, stdout))
}

fn resume_name(
    progress: MgProgress,
    result: NameLookupResult,
) -> Result<(MgProgress, String), MgError> {
    let mut stdout = String::new();
    let writer = PrintWriter::CollectString(&mut stdout);
    let next = match progress {
        MgProgress::NoLimit(progress) => match progress {
            RunProgress::NameLookup(lookup) => lookup.resume(result, writer),
            _ => {
                return Err(ffi_error(
                    "RuntimeError",
                    "progress state is not a name lookup",
                ));
            }
        }
        .map(MgProgress::NoLimit),
        MgProgress::Limited(progress) => match progress {
            RunProgress::NameLookup(lookup) => lookup.resume(result, writer),
            _ => {
                return Err(ffi_error(
                    "RuntimeError",
                    "progress state is not a name lookup",
                ));
            }
        }
        .map(MgProgress::Limited),
    }
    .map_err(|exc| from_monty_error(&exc))?;
    Ok((next, stdout))
}

fn resume_futures(
    progress: MgProgress,
    results: Vec<(u32, ExtFunctionResult)>,
) -> Result<(MgProgress, String), MgError> {
    let mut stdout = String::new();
    let writer = PrintWriter::CollectString(&mut stdout);
    let next = match progress {
        MgProgress::NoLimit(progress) => match progress {
            RunProgress::ResolveFutures(state) => state.resume(results, writer),
            _ => {
                return Err(ffi_error(
                    "RuntimeError",
                    "progress state is not waiting on futures",
                ));
            }
        }
        .map(MgProgress::NoLimit),
        MgProgress::Limited(progress) => match progress {
            RunProgress::ResolveFutures(state) => state.resume(results, writer),
            _ => {
                return Err(ffi_error(
                    "RuntimeError",
                    "progress state is not waiting on futures",
                ));
            }
        }
        .map(MgProgress::Limited),
    }
    .map_err(|exc| from_monty_error(&exc))?;
    Ok((next, stdout))
}

const fn progress_kind(progress: &MgProgress) -> u32 {
    match progress {
        MgProgress::NoLimit(progress) => match progress {
            RunProgress::FunctionCall(_) => PROGRESS_FUNCTION_CALL,
            RunProgress::OsCall(_) => PROGRESS_OS_CALL,
            RunProgress::ResolveFutures(_) => PROGRESS_RESOLVE_FUTURES,
            RunProgress::NameLookup(_) => PROGRESS_NAME_LOOKUP,
            RunProgress::Complete(_) => PROGRESS_COMPLETE,
        },
        MgProgress::Limited(progress) => match progress {
            RunProgress::FunctionCall(_) => PROGRESS_FUNCTION_CALL,
            RunProgress::OsCall(_) => PROGRESS_OS_CALL,
            RunProgress::ResolveFutures(_) => PROGRESS_RESOLVE_FUTURES,
            RunProgress::NameLookup(_) => PROGRESS_NAME_LOOKUP,
            RunProgress::Complete(_) => PROGRESS_COMPLETE,
        },
    }
}

fn with_resolve_futures<R>(progress: *const MgProgress, f: impl FnOnce(&[u32]) -> R) -> Option<R> {
    if progress.is_null() {
        return None;
    }
    // SAFETY: handle validity is owned by the Go side contract.
    match unsafe { &*progress } {
        MgProgress::NoLimit(RunProgress::ResolveFutures(state)) => {
            Some(f(state.pending_call_ids()))
        }
        MgProgress::Limited(RunProgress::ResolveFutures(state)) => {
            Some(f(state.pending_call_ids()))
        }
        _ => None,
    }
}

fn free_raw_pair(pair: &mut MgRawPair) {
    free_raw_value(&mut pair.key);
    free_raw_value(&mut pair.value);
}

fn free_raw_value(value: &mut MgRawValue) {
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

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_error_free(error: *mut MgError) {
    if !error.is_null() {
        // SAFETY: error handles are allocated with Box::into_raw in this crate.
        unsafe { drop(Box::from_raw(error)) };
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_error_type(error: *const MgError, out: *mut MgBytes) -> i32 {
    guard(ptr::null_mut(), || {
        if error.is_null() {
            return Err(ffi_error("TypeError", "error handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        write_string(out, unsafe { &(*error).exc_type })
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_error_message(error: *const MgError, out: *mut MgBytes) -> i32 {
    guard(ptr::null_mut(), || {
        if error.is_null() {
            return Err(ffi_error("TypeError", "error handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        write_string(out, unsafe { &(*error).message })
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_error_display(error: *const MgError, out: *mut MgBytes) -> i32 {
    guard(ptr::null_mut(), || {
        if error.is_null() {
            return Err(ffi_error("TypeError", "error handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        write_string(out, unsafe { &(*error).display })
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_type_check(
    code_ptr: *const u8,
    code_len: usize,
    script_name_ptr: *const u8,
    script_name_len: usize,
    stubs_ptr: *const u8,
    stubs_len: usize,
    stubs_name_ptr: *const u8,
    stubs_name_len: usize,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        let code = str_arg(code_ptr, code_len)?;
        let script_name = str_arg(script_name_ptr, script_name_len)?;
        let stubs = str_arg(stubs_ptr, stubs_len)?;
        let stubs_name = str_arg(stubs_name_ptr, stubs_name_len)?;
        let stubs_file = (!stubs.is_empty()).then(|| SourceFile::new(stubs, stubs_name));
        match type_check(&SourceFile::new(code, script_name), stubs_file.as_ref()) {
            Ok(None) => Ok(()),
            Ok(Some(diagnostics)) => Err(ffi_error("MontyTypingError", diagnostics.to_string())),
            Err(message) => Err(ffi_error("RuntimeError", message)),
        }
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_mount_new(
    args: *const MgMountNewArgs,
    out_mount: *mut *mut MgMount,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if args.is_null() {
            return Err(ffi_error("TypeError", "mount args pointer is null"));
        }
        if out_mount.is_null() {
            return Err(ffi_error("TypeError", "mount output pointer is null"));
        }
        // SAFETY: args is checked for null above and only read during this call.
        let args = unsafe { &*args };
        let virtual_path = as_str(args.virtual_path)?;
        let host_path = as_str(args.host_path)?;
        let mode = match args.mode {
            0 => MontyMountMode::ReadOnly,
            1 => MontyMountMode::ReadWrite,
            2 => MontyMountMode::from_mode_str("overlay")
                .map_err(|err| ffi_error("ValueError", err))?,
            _ => {
                return Err(ffi_error(
                    "ValueError",
                    "mount mode must be read-only, read-write, or overlay",
                ));
            }
        };
        let limit = (args.has_write_bytes_limit != 0).then_some(args.write_bytes_limit);
        let mount = MontyMount::new(virtual_path, host_path, mode, limit)
            .map_err(|err| from_monty_error(&err.into_exception()))?;
        // SAFETY: out_mount is checked for null above.
        unsafe {
            *out_mount = Box::into_raw(Box::new(MgMount {
                slot: Arc::new(Mutex::new(Some(mount))),
            }));
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_mount_free(mount: *mut MgMount) {
    if !mount.is_null() {
        // SAFETY: mount handles are allocated with Box::into_raw in this crate.
        unsafe { drop(Box::from_raw(mount)) };
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_mount_handle_os_call(
    args: *const MgMountCallArgs,
    out: *mut MgMountOutput,
) -> i32 {
    let out_error = if out.is_null() {
        ptr::null_mut()
    } else {
        // SAFETY: out is non-null and points to a caller-provided output struct.
        unsafe { ptr::addr_of_mut!((*out).error) }
    };
    guard(out_error, || {
        if args.is_null() {
            return Err(ffi_error("TypeError", "mount call args pointer is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "mount output pointer is null"));
        }
        // SAFETY: args is checked for null above and only read during this call.
        let args = unsafe { &*args };
        if args.mounts.is_null() && args.mount_count != 0 {
            return Err(ffi_error(
                "TypeError",
                "non-empty mount array has null pointer",
            ));
        }
        // SAFETY: out is checked for null above.
        unsafe {
            (*out).handled = 0;
            (*out).value = ptr::null_mut();
            (*out).error = ptr::null_mut();
        }

        let function = as_str(args.function)?
            .parse::<OsFunction>()
            .map_err(|_| ffi_error("ValueError", "unknown OS function"))?;
        let call_args = read_values(args.args, args.arg_count)?;
        let kwargs = read_value_pairs(args.kwarg_keys, args.kwarg_values, args.kwarg_count)?;

        let mount_handles = if args.mount_count == 0 {
            &[][..]
        } else {
            // SAFETY: caller promises mounts points to mount_count handles.
            unsafe { slice::from_raw_parts(args.mounts, args.mount_count) }
        };
        let mut slots = Vec::with_capacity(mount_handles.len());
        for mount in mount_handles {
            if mount.is_null() {
                return Err(ffi_error("TypeError", "mount handle is null"));
            }
            // SAFETY: mount handle validity is owned by the Go side contract.
            slots.push(unsafe { (*(*mount)).slot.clone() });
        }

        let mut table =
            MountTable::take_shared_mounts(&slots).map_err(|err| ffi_error("RuntimeError", err))?;
        let result = table.handle_os_call(function, &call_args, &kwargs);
        table.put_back_shared_mounts(&slots);

        match result {
            None => Ok(()),
            Some(Ok(object)) => {
                // SAFETY: out is checked for null above.
                unsafe {
                    (*out).handled = 1;
                    (*out).value = Box::into_raw(Box::new(MgValue { object }));
                }
                Ok(())
            }
            Some(Err(err)) => Err(from_monty_error(&err.into_exception())),
        }
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_repl_new(
    args: *const MgReplNewArgs,
    out_repl: *mut *mut MgRepl,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if args.is_null() {
            return Err(ffi_error("TypeError", "REPL args pointer is null"));
        }
        if out_repl.is_null() {
            return Err(ffi_error("TypeError", "REPL output pointer is null"));
        }
        // SAFETY: args is checked for null above and only read during this call.
        let args = unsafe { &*args };
        let script_name = as_str(args.script_name)?;
        let repl = limits_from_raw(args.limits).map_or_else(
            || MgRepl::NoLimit(MontyRepl::new(script_name, NoLimitTracker)),
            |limits| MgRepl::Limited(MontyRepl::new(script_name, LimitedTracker::new(limits))),
        );
        // SAFETY: out_repl is checked for null above.
        unsafe { *out_repl = Box::into_raw(Box::new(repl)) };
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_repl_free(repl: *mut MgRepl) {
    if !repl.is_null() {
        // SAFETY: repl handles are allocated with Box::into_raw in this crate.
        unsafe { drop(Box::from_raw(repl)) };
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_repl_dump(
    repl: *const MgRepl,
    out: *mut MgBytes,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if repl.is_null() {
            return Err(ffi_error("TypeError", "REPL handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        let bytes = postcard::to_allocvec(unsafe { &*repl })
            .map_err(|err| ffi_error("SerializationError", err.to_string()))?;
        write_bytes(out, &bytes)
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_repl_load(
    ptr: *const u8,
    len: usize,
    out_repl: *mut *mut MgRepl,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if out_repl.is_null() {
            return Err(ffi_error("TypeError", "REPL output pointer is null"));
        }
        let bytes = as_bytes(ptr, len)?;
        let repl: MgRepl = postcard::from_bytes(bytes)
            .map_err(|err| ffi_error("SerializationError", err.to_string()))?;
        // SAFETY: out_repl is checked for null above.
        unsafe { *out_repl = Box::into_raw(Box::new(repl)) };
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_repl_feed_run(
    repl: *mut MgRepl,
    args: *const MgReplFeedRunArgs,
    out: *mut MgRunOutput,
) -> i32 {
    let out_error = if out.is_null() {
        ptr::null_mut()
    } else {
        // SAFETY: out is non-null and points to a caller-provided output struct.
        unsafe { ptr::addr_of_mut!((*out).error) }
    };
    guard(out_error, || {
        if repl.is_null() {
            return Err(ffi_error("TypeError", "REPL handle is null"));
        }
        if args.is_null() {
            return Err(ffi_error("TypeError", "REPL feed args pointer is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        // SAFETY: args is checked for null above and only read during this call.
        let args = unsafe { &*args };
        let code = as_str(args.code)?;
        let inputs = read_named_values(args.input_names, args.input_values, args.input_count)?;
        let mut stdout = String::new();
        let writer = PrintWriter::CollectString(&mut stdout);
        // SAFETY: handle validity is owned by the Go side contract.
        let value = match unsafe { &mut *repl } {
            MgRepl::NoLimit(repl) => repl.feed_run(code, inputs, writer),
            MgRepl::Limited(repl) => repl.feed_run(code, inputs, writer),
        }
        .map_err(|exc| from_monty_error(&exc))?;
        // SAFETY: out is checked for null above.
        unsafe {
            write_owned_string(ptr::addr_of_mut!((*out).print), stdout)?;
            (*out).value = Box::into_raw(Box::new(MgValue { object: value }));
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_repl_call_function(
    repl: *mut MgRepl,
    args: *const MgReplCallArgs,
    out: *mut MgRunOutput,
) -> i32 {
    let out_error = if out.is_null() {
        ptr::null_mut()
    } else {
        // SAFETY: out is non-null and points to a caller-provided output struct.
        unsafe { ptr::addr_of_mut!((*out).error) }
    };
    guard(out_error, || {
        if repl.is_null() {
            return Err(ffi_error("TypeError", "REPL handle is null"));
        }
        if args.is_null() {
            return Err(ffi_error("TypeError", "REPL call args pointer is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        // SAFETY: args is checked for null above and only read during this call.
        let args = unsafe { &*args };
        let name = as_str(args.name)?;
        let args = read_values(args.args, args.arg_count)?;
        let mut stdout = String::new();
        let writer = PrintWriter::CollectString(&mut stdout);
        // SAFETY: handle validity is owned by the Go side contract.
        let value = match unsafe { &mut *repl } {
            MgRepl::NoLimit(repl) => repl.call_function(name, args, writer),
            MgRepl::Limited(repl) => repl.call_function(name, args, writer),
        }
        .map_err(|exc| from_monty_error(&exc))?;
        // SAFETY: out is checked for null above.
        unsafe {
            write_owned_string(ptr::addr_of_mut!((*out).print), stdout)?;
            (*out).value = Box::into_raw(Box::new(MgValue { object: value }));
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_repl_function_names(
    repl: *const MgRepl,
    out_names: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if repl.is_null() {
            return Err(ffi_error("TypeError", "REPL handle is null"));
        }
        if out_names.is_null() {
            return Err(ffi_error(
                "TypeError",
                "function names output pointer is null",
            ));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        let names: Vec<String> = match unsafe { &*repl } {
            MgRepl::NoLimit(repl) => repl
                .function_names()
                .into_iter()
                .map(str::to_owned)
                .collect(),
            MgRepl::Limited(repl) => repl
                .function_names()
                .into_iter()
                .map(str::to_owned)
                .collect(),
        };
        // SAFETY: out_names is checked for null above.
        unsafe { *out_names = string_list_value(&names) };
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_repl_has_function(
    repl: *const MgRepl,
    name_ptr: *const u8,
    name_len: usize,
) -> u8 {
    if repl.is_null() {
        return 0;
    }
    let Ok(name) = str_arg(name_ptr, name_len) else {
        return 0;
    };
    // SAFETY: handle validity is owned by the Go side contract.
    let has_function = match unsafe { &*repl } {
        MgRepl::NoLimit(repl) => repl.has_function(name),
        MgRepl::Limited(repl) => repl.has_function(name),
    };
    u8::from(has_function)
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_repl_continuation_mode(ptr: *const u8, len: usize) -> u32 {
    let Ok(source) = str_arg(ptr, len) else {
        return 0;
    };
    match detect_repl_continuation_mode(source) {
        ReplContinuationMode::Complete => 0,
        ReplContinuationMode::IncompleteImplicit => 1,
        ReplContinuationMode::IncompleteBlock => 2,
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_program_compile(
    args: *const MgProgramCompileArgs,
    out_program: *mut *mut MgProgram,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if args.is_null() {
            return Err(ffi_error("TypeError", "compile args pointer is null"));
        }
        if out_program.is_null() {
            return Err(ffi_error("TypeError", "program output pointer is null"));
        }
        // SAFETY: args is checked for null above and only read during this call.
        let args = unsafe { &*args };
        let code = as_str(args.code)?.to_owned();
        let script_name = as_str(args.script_name)?.to_owned();
        let names = if args.input_names.is_null() {
            if args.input_count == 0 {
                Vec::new()
            } else {
                return Err(ffi_error(
                    "TypeError",
                    "non-empty input-name array has null pointer",
                ));
            }
        } else {
            // SAFETY: caller promises input_names points to input_count MgStr entries.
            unsafe { slice::from_raw_parts(args.input_names, args.input_count) }
                .iter()
                .map(|name| as_str(*name).map(str::to_owned))
                .collect::<Result<Vec<_>, _>>()?
        };
        let runner = MontyRun::new(code, &script_name, names.clone())
            .map_err(|exc| from_monty_error(&exc))?;
        let program = MgProgram {
            runner,
            script_name,
            input_names: names,
        };
        // SAFETY: out_program is checked for null above.
        unsafe { *out_program = Box::into_raw(Box::new(program)) };
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_program_free(program: *mut MgProgram) {
    if !program.is_null() {
        // SAFETY: program handles are allocated with Box::into_raw in this crate.
        unsafe { drop(Box::from_raw(program)) };
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_program_dump(
    program: *const MgProgram,
    out: *mut MgBytes,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if program.is_null() {
            return Err(ffi_error("TypeError", "program handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        let program = unsafe { &*program };
        let stored = StoredProgram {
            runner: program.runner.clone(),
            script_name: program.script_name.clone(),
            input_names: program.input_names.clone(),
        };
        let bytes = postcard::to_allocvec(&stored)
            .map_err(|err| ffi_error("SerializationError", err.to_string()))?;
        write_bytes(out, &bytes)
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_program_code(
    program: *const MgProgram,
    out: *mut MgBytes,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if program.is_null() {
            return Err(ffi_error("TypeError", "program handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        write_string(out, unsafe { (*program).runner.code() })
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_program_script_name(
    program: *const MgProgram,
    out: *mut MgBytes,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if program.is_null() {
            return Err(ffi_error("TypeError", "program handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        write_string(out, unsafe { &(*program).script_name })
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_program_input_names(
    program: *const MgProgram,
    out_names: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if program.is_null() {
            return Err(ffi_error("TypeError", "program handle is null"));
        }
        if out_names.is_null() {
            return Err(ffi_error("TypeError", "input names output pointer is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        let names = unsafe { &(*program).input_names };
        // SAFETY: out_names is checked for null above.
        unsafe { *out_names = string_list_value(names) };
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_program_load(
    ptr: *const u8,
    len: usize,
    out_program: *mut *mut MgProgram,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if out_program.is_null() {
            return Err(ffi_error("TypeError", "program output pointer is null"));
        }
        let bytes = as_bytes(ptr, len)?;
        let stored: StoredProgram = postcard::from_bytes(bytes)
            .map_err(|err| ffi_error("SerializationError", err.to_string()))?;
        let program = MgProgram {
            runner: stored.runner,
            script_name: stored.script_name,
            input_names: stored.input_names,
        };
        // SAFETY: out_program is checked for null above.
        unsafe { *out_program = Box::into_raw(Box::new(program)) };
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_program_start_raw(
    program: *const MgProgram,
    inputs: *mut MgRawValue,
    input_count: usize,
    limits: *const MgLimits,
    out: *mut MgStartOutput,
) -> i32 {
    let out_error = if out.is_null() {
        ptr::null_mut()
    } else {
        // SAFETY: out is non-null and points to a caller-provided output struct.
        unsafe { ptr::addr_of_mut!((*out).error) }
    };
    guard(out_error, || {
        if program.is_null() {
            return Err(ffi_error("TypeError", "program handle is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "progress output pointer is null"));
        }
        let input_values = read_raw_values(inputs, input_count)?;
        // SAFETY: handle validity is owned by the Go side contract.
        let (progress, stdout) =
            start_with_print(unsafe { &*program }, input_values, limits_from_raw(limits))?;
        // SAFETY: out is checked for null above.
        unsafe {
            write_owned_string(ptr::addr_of_mut!((*out).print), stdout)?;
            (*out).progress = Box::into_raw(Box::new(progress));
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_program_start_raw_snapshot(
    program: *const MgProgram,
    inputs: *mut MgRawValue,
    input_count: usize,
    limits: *const MgLimits,
    out: *mut MgProgressSnapshotOutput,
) -> i32 {
    guard(progress_snapshot_output_error(out), || {
        if program.is_null() {
            return Err(ffi_error("TypeError", "program handle is null"));
        }
        if out.is_null() {
            return Err(ffi_error(
                "TypeError",
                "progress snapshot output pointer is null",
            ));
        }
        let input_values = read_raw_values(inputs, input_count)?;
        // SAFETY: handle validity is owned by the Go side contract.
        let (progress, stdout) =
            start_with_print(unsafe { &*program }, input_values, limits_from_raw(limits))?;
        // SAFETY: out is checked for null above.
        unsafe { write_progress_snapshot_output(out, progress, stdout) }
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_program_run_raw(
    program: *const MgProgram,
    inputs: *mut MgRawValue,
    input_count: usize,
    limits: *const MgLimits,
    out: *mut MgRunRawOutput,
) -> i32 {
    let out_error = if out.is_null() {
        ptr::null_mut()
    } else {
        // SAFETY: out is non-null and points to a caller-provided output struct.
        unsafe { ptr::addr_of_mut!((*out).error) }
    };
    guard(out_error, || {
        if program.is_null() {
            return Err(ffi_error("TypeError", "program handle is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "raw value output pointer is null"));
        }
        let input_values = read_raw_values(inputs, input_count)?;
        // SAFETY: handle validity is owned by the Go side contract.
        let (value, stdout) =
            run_with_print(unsafe { &*program }, input_values, limits_from_raw(limits))?;
        // SAFETY: out is checked for null above.
        unsafe {
            write_owned_string(ptr::addr_of_mut!((*out).print), stdout)?;
            write_raw_value(ptr::addr_of_mut!((*out).value), value)
        }
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_program_run_host_raw(
    program: *const MgProgram,
    inputs: *mut MgRawValue,
    input_count: usize,
    limits: *const MgLimits,
    host_names: *const MgStr,
    host_name_count: usize,
    callback: MgHostFunctionCallback,
    user_data: usize,
    out: *mut MgRunRawOutput,
) -> i32 {
    let out_error = if out.is_null() {
        ptr::null_mut()
    } else {
        // SAFETY: out is non-null and points to a caller-provided output struct.
        unsafe { ptr::addr_of_mut!((*out).error) }
    };
    guard(out_error, || {
        if program.is_null() {
            return Err(ffi_error("TypeError", "program handle is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "raw value output pointer is null"));
        }
        let input_values = read_raw_values(inputs, input_count)?;
        let names = read_string_list(host_names, host_name_count)?;
        // SAFETY: handle validity is owned by the Go side contract.
        let (value, stdout) = run_with_host_callback(
            unsafe { &*program },
            input_values,
            limits_from_raw(limits),
            &names,
            callback,
            user_data,
        )?;
        // SAFETY: out is checked for null above.
        unsafe {
            write_owned_string(ptr::addr_of_mut!((*out).print), stdout)?;
            write_raw_value(ptr::addr_of_mut!((*out).value), value)
        }
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_program_run_fast_raw(
    program: *const MgProgram,
    inputs: *mut MgRawValue,
    input_count: usize,
    limits: *const MgLimits,
    out: *mut MgRunFastOutput,
) -> i32 {
    let out_error = if out.is_null() {
        ptr::null_mut()
    } else {
        // SAFETY: out is non-null and points to a caller-provided output struct.
        unsafe { ptr::addr_of_mut!((*out).error) }
    };
    guard(out_error, || {
        if program.is_null() {
            return Err(ffi_error("TypeError", "program handle is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "fast value output pointer is null"));
        }
        let input_values = read_raw_values(inputs, input_count)?;
        // SAFETY: handle validity is owned by the Go side contract.
        let (value, stdout) =
            run_with_print(unsafe { &*program }, input_values, limits_from_raw(limits))?;
        // SAFETY: out is checked for null above.
        unsafe { write_fast_output(out, value, stdout) }
    })
}

/// Compile a program, run it once, and free it — all in a single FFI call.
///
/// Saves two cgocall hops compared to separate compile/run/free calls and
/// avoids allocating a long-lived `MgProgram` on the Go side. The result is
/// written into `out` using the same format as `mg_program_run_fast_raw`.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_program_compile_run_fast_raw(
    args: *const MgCompileRunFastRawArgs,
    out: *mut MgRunFastOutput,
) -> i32 {
    let out_error = if out.is_null() {
        ptr::null_mut()
    } else {
        // SAFETY: out is non-null and points to a caller-provided output struct.
        unsafe { ptr::addr_of_mut!((*out).error) }
    };
    guard(out_error, || {
        if args.is_null() {
            return Err(ffi_error("TypeError", "compile-run args pointer is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "fast value output pointer is null"));
        }
        // SAFETY: args was checked for null and is only read during this call.
        let args = unsafe { &*args };
        let code = as_str(args.code)?.to_owned();
        let script_name = as_str(args.script_name)?.to_owned();
        let names = if args.input_names.is_null() {
            if args.input_count == 0 {
                Vec::new()
            } else {
                return Err(ffi_error(
                    "TypeError",
                    "non-empty input-name array has null pointer",
                ));
            }
        } else {
            // SAFETY: caller promises input_names points to input_count MgStr entries.
            unsafe { slice::from_raw_parts(args.input_names, args.input_count) }
                .iter()
                .map(|name| as_str(*name).map(str::to_owned))
                .collect::<Result<Vec<_>, _>>()?
        };
        let runner = MontyRun::new(code, &script_name, names.clone())
            .map_err(|exc| from_monty_error(&exc))?;
        let program = MgProgram {
            runner,
            script_name,
            input_names: names,
        };
        let input_values = read_raw_values(args.input_values, args.input_value_count)?;
        let (value, stdout) = run_with_print(&program, input_values, limits_from_raw(args.limits))?;
        // SAFETY: out was checked for null above.
        unsafe { write_fast_output(out, value, stdout) }
    })
}

/// Populate an `MgRunFastOutput` with the result of a run. Scalar values are
/// returned via `FAST_FORMAT_RAW` (no bytes allocation) so the Go side can
/// decode without freeing a Rust-owned buffer. Compound values fall through
/// to the flat byte stream when it can be produced, and to the owned-handle
/// `MgRawValue` otherwise.
///
/// # Safety
/// `out` must point to a writable `MgRunFastOutput`.
unsafe fn write_fast_output(
    out: *mut MgRunFastOutput,
    value: MontyObject,
    stdout: String,
) -> Result<(), MgError> {
    // SAFETY: out is non-null by this function's safety contract and is only
    // written during this call.
    unsafe {
        (*out).format = FAST_FORMAT_RAW;
        (*out).bytes_in_scratch = 0;
        (*out).value = raw_value(KIND_INVALID);
        (*out).bytes = MgBytes {
            ptr: ptr::null_mut(),
            len: 0,
        };
        write_owned_string(ptr::addr_of_mut!((*out).print), stdout)?;
    }
    if let Some(raw) = fast_scalar_raw(&value) {
        // SAFETY: out is non-null by contract.
        unsafe { (*out).value = raw };
        return Ok(());
    }
    let mut bytes = Vec::new();
    match write_flat_value(&mut bytes, &value) {
        Ok(()) => {
            // SAFETY: out is non-null by contract; scratch ownership is handled
            // by write_flat_output below.
            unsafe {
                (*out).format = FAST_FORMAT_FLAT;
                write_flat_output(out, bytes)
            }
        }
        // SAFETY: out is non-null by contract.
        Err(_) => unsafe { write_raw_value(ptr::addr_of_mut!((*out).value), value) },
    }
}

/// Hand the flat byte stream back to the caller. Small payloads are copied
/// into the inline `scratch` so the Go side does not need a `mg_bytes_free`
/// cgocall; larger payloads are leaked through `write_owned_bytes` as before.
///
/// # Safety
/// `out` must point to a writable `MgRunFastOutput`.
unsafe fn write_flat_output(out: *mut MgRunFastOutput, bytes: Vec<u8>) -> Result<(), MgError> {
    let len = bytes.len();
    if len <= FAST_SCRATCH_CAP {
        // SAFETY: out is non-null and scratch is a fixed-size array.
        unsafe {
            let scratch = ptr::addr_of_mut!((*out).scratch).cast::<u8>();
            ptr::copy_nonoverlapping(bytes.as_ptr(), scratch, len);
            (*out).bytes = MgBytes { ptr: scratch, len };
            (*out).bytes_in_scratch = 1;
        }
        return Ok(());
    }
    // SAFETY: out is non-null; bytes ownership transfers to the caller.
    unsafe { write_owned_bytes(ptr::addr_of_mut!((*out).bytes), bytes) }
}

/// Encode scalar results directly into an `MgRawValue` so the Go side can read
/// them without touching a heap-allocated bytes buffer. Returns `None` for
/// values that need either flat encoding or a Rust-owned handle.
fn fast_scalar_raw(object: &MontyObject) -> Option<MgRawValue> {
    match object {
        MontyObject::Ellipsis => Some(raw_value(KIND_ELLIPSIS)),
        MontyObject::None => Some(raw_value(KIND_NONE)),
        MontyObject::Bool(value) => {
            let mut raw = raw_value(KIND_BOOL);
            raw.bool_value = u8::from(*value);
            Some(raw)
        }
        MontyObject::Int(value) => {
            let mut raw = raw_value(KIND_INT);
            raw.int_value = *value;
            Some(raw)
        }
        MontyObject::Float(value) => {
            let mut raw = raw_value(KIND_FLOAT);
            raw.float_value = *value;
            Some(raw)
        }
        _ => None,
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_program_run_json_raw(
    program: *const MgProgram,
    inputs: *mut MgRawValue,
    input_count: usize,
    limits: *const MgLimits,
    out: *mut MgRunJsonOutput,
) -> i32 {
    let out_error = if out.is_null() {
        ptr::null_mut()
    } else {
        // SAFETY: out is non-null and points to a caller-provided output struct.
        unsafe { ptr::addr_of_mut!((*out).error) }
    };
    guard(out_error, || {
        if program.is_null() {
            return Err(ffi_error("TypeError", "program handle is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "json output pointer is null"));
        }
        let input_values = read_raw_values(inputs, input_count)?;
        // SAFETY: handle validity is owned by the Go side contract.
        let (value, stdout) =
            run_with_print(unsafe { &*program }, input_values, limits_from_raw(limits))?;
        let json = serde_json::to_vec(&JsonMontyObject(&value))
            .map_err(|err| ffi_error("SerializationError", err.to_string()))?;
        // SAFETY: out is checked for null above.
        unsafe {
            write_owned_bytes(ptr::addr_of_mut!((*out).value), json)?;
            write_owned_string(ptr::addr_of_mut!((*out).print), stdout)
        }
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_free(value: *mut MgValue) {
    if !value.is_null() {
        // SAFETY: value handles are allocated with Box::into_raw in this crate.
        unsafe { drop(Box::from_raw(value)) };
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_json(
    value: *const MgValue,
    out: *mut MgBytes,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        let bytes = serde_json::to_vec(&JsonMontyObject(unsafe { &(*value).object }))
            .map_err(|err| ffi_error("SerializationError", err.to_string()))?;
        write_bytes(out, &bytes)
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_ellipsis() -> *mut MgValue {
    Box::into_raw(Box::new(MgValue {
        object: MontyObject::Ellipsis,
    }))
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_none() -> *mut MgValue {
    Box::into_raw(Box::new(MgValue {
        object: MontyObject::None,
    }))
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_bool(value: u8) -> *mut MgValue {
    Box::into_raw(Box::new(MgValue {
        object: MontyObject::Bool(value != 0),
    }))
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_int(value: i64) -> *mut MgValue {
    Box::into_raw(Box::new(MgValue {
        object: MontyObject::Int(value),
    }))
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_big_int(
    value_ptr: *const u8,
    value_len: usize,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        let value = str_arg(value_ptr, value_len)?;
        let integer = value
            .parse::<BigInt>()
            .map_err(|err| ffi_error("ValueError", err.to_string()))?;
        // SAFETY: out is checked for null above.
        unsafe {
            *out = Box::into_raw(Box::new(MgValue {
                object: MontyObject::BigInt(integer),
            }));
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_float(value: f64) -> *mut MgValue {
    Box::into_raw(Box::new(MgValue {
        object: MontyObject::Float(value),
    }))
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_string(
    value_ptr: *const u8,
    value_len: usize,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        let value = str_arg(value_ptr, value_len)?.to_owned();
        // SAFETY: out is checked for null above.
        unsafe {
            *out = Box::into_raw(Box::new(MgValue {
                object: MontyObject::String(value),
            }));
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_path(
    value_ptr: *const u8,
    value_len: usize,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        let value = str_arg(value_ptr, value_len)?.to_owned();
        // SAFETY: out is checked for null above.
        unsafe {
            *out = Box::into_raw(Box::new(MgValue {
                object: MontyObject::Path(value),
            }));
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_bytes(
    ptr: *const u8,
    len: usize,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        let bytes = as_bytes(ptr, len)?.to_vec();
        // SAFETY: out is checked for null above.
        unsafe {
            *out = Box::into_raw(Box::new(MgValue {
                object: MontyObject::Bytes(bytes),
            }));
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_list_raw(
    values: *mut MgRawValue,
    len: usize,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        let values = read_raw_values(values, len)?;
        write_value_handle(out, MontyObject::List(values))
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_tuple_raw(
    values: *mut MgRawValue,
    len: usize,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        let values = read_raw_values(values, len)?;
        write_value_handle(out, MontyObject::Tuple(values))
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_named_tuple_raw(
    type_name_ptr: *const u8,
    type_name_len: usize,
    field_names: *const MgStr,
    values: *mut MgRawValue,
    len: usize,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        let type_name = str_arg(type_name_ptr, type_name_len)?.to_owned();
        let field_names = read_string_list(field_names, len)?;
        let values = read_raw_values(values, len)?;
        write_value_handle(
            out,
            MontyObject::NamedTuple {
                type_name,
                field_names,
                values,
            },
        )
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_set_raw(
    values: *mut MgRawValue,
    len: usize,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        let values = read_raw_values(values, len)?;
        write_value_handle(out, MontyObject::Set(values))
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_frozen_set_raw(
    values: *mut MgRawValue,
    len: usize,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        let values = read_raw_values(values, len)?;
        write_value_handle(out, MontyObject::FrozenSet(values))
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_dict_raw(
    pairs: *mut MgRawPair,
    len: usize,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        let pairs = read_raw_pairs(pairs, len)?;
        write_value_handle(out, MontyObject::Dict(DictPairs::from(pairs)))
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_date(
    value: *const MgDate,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        if value.is_null() {
            return Err(ffi_error("TypeError", "date pointer is null"));
        }
        // SAFETY: value is checked for null above and only read during this call.
        let value = unsafe { *value };
        // SAFETY: out is checked for null above.
        unsafe {
            *out = Box::into_raw(Box::new(MgValue {
                object: MontyObject::Date(monty_date_from_raw(value)),
            }));
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_datetime(
    value: *const MgDateTime,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        if value.is_null() {
            return Err(ffi_error("TypeError", "datetime pointer is null"));
        }
        // SAFETY: value is checked for null above and only read during this call.
        let value = monty_datetime_from_raw(unsafe { &*value })?;
        // SAFETY: out is checked for null above.
        unsafe {
            *out = Box::into_raw(Box::new(MgValue {
                object: MontyObject::DateTime(value),
            }));
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_timedelta(
    value: *const MgTimeDelta,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        if value.is_null() {
            return Err(ffi_error("TypeError", "timedelta pointer is null"));
        }
        // SAFETY: value is checked for null above and only read during this call.
        let value = unsafe { *value };
        // SAFETY: out is checked for null above.
        unsafe {
            *out = Box::into_raw(Box::new(MgValue {
                object: MontyObject::TimeDelta(monty_timedelta_from_raw(value)),
            }));
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_timezone(
    value: *const MgTimeZone,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        if value.is_null() {
            return Err(ffi_error("TypeError", "timezone pointer is null"));
        }
        // SAFETY: value is checked for null above and only read during this call.
        let value = monty_timezone_from_raw(unsafe { &*value })?;
        // SAFETY: out is checked for null above.
        unsafe {
            *out = Box::into_raw(Box::new(MgValue {
                object: MontyObject::TimeZone(value),
            }));
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_dataclass_raw(
    args: *mut MgDataclassRawArgs,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if args.is_null() {
            return Err(ffi_error("TypeError", "dataclass args pointer is null"));
        }
        // SAFETY: args is checked for null above and only read during this call.
        let args = unsafe { &mut *args };
        let name = as_str(args.name)?.to_owned();
        let field_names = read_string_list(args.field_names, args.field_count)?;
        let attrs = DictPairs::from(read_raw_pairs(args.attrs, args.attr_count)?);
        write_value_handle(
            out,
            MontyObject::Dataclass {
                name,
                type_id: args.type_id,
                field_names,
                attrs,
                frozen: args.frozen != 0,
            },
        )
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_function(
    name_ptr: *const u8,
    name_len: usize,
    doc_ptr: *const u8,
    doc_len: usize,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        let name = str_arg(name_ptr, name_len)?.to_owned();
        let doc = str_arg(doc_ptr, doc_len)?;
        let docstring = (!doc.is_empty()).then(|| doc.to_owned());
        // SAFETY: out is checked for null above.
        unsafe {
            *out = Box::into_raw(Box::new(MgValue {
                object: MontyObject::Function { name, docstring },
            }));
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_exception(
    exc_type_ptr: *const u8,
    exc_type_len: usize,
    message_ptr: *const u8,
    message_len: usize,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        let exc_type = str_arg(exc_type_ptr, exc_type_len)?
            .parse::<ExcType>()
            .map_err(|_| ffi_error("ValueError", "unknown exception type"))?;
        let message = str_arg(message_ptr, message_len)?;
        let arg = (!message.is_empty()).then(|| message.to_owned());
        // SAFETY: out is checked for null above.
        unsafe {
            *out = Box::into_raw(Box::new(MgValue {
                object: MontyObject::Exception { exc_type, arg },
            }));
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_exception_type(
    value: *const MgValue,
    out: *mut MgBytes,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        match unsafe { &(*value).object } {
            MontyObject::Exception { exc_type, .. } => write_string(out, &exc_type.to_string()),
            _ => Err(ffi_error("TypeError", "value is not an exception")),
        }
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_exception_message(
    value: *const MgValue,
    out: *mut MgBytes,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        match unsafe { &(*value).object } {
            MontyObject::Exception { arg, .. } => write_string(out, arg.as_deref().unwrap_or("")),
            _ => Err(ffi_error("TypeError", "value is not an exception")),
        }
    })
}

#[unsafe(no_mangle)]
pub const unsafe extern "C" fn mg_value_kind(value: *const MgValue) -> u32 {
    if value.is_null() {
        return KIND_INVALID;
    }
    // SAFETY: handle validity is owned by the Go side contract.
    object_kind(unsafe { &(*value).object })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_bool_get(value: *const MgValue) -> u8 {
    if value.is_null() {
        return 0;
    }
    // SAFETY: handle validity is owned by the Go side contract.
    match unsafe { &(*value).object } {
        MontyObject::Bool(value) => u8::from(*value),
        _ => 0,
    }
}

#[unsafe(no_mangle)]
#[allow(clippy::missing_const_for_fn)]
pub unsafe extern "C" fn mg_value_int_get(value: *const MgValue) -> i64 {
    if value.is_null() {
        return 0;
    }
    // SAFETY: handle validity is owned by the Go side contract.
    match unsafe { &(*value).object } {
        MontyObject::Int(value) => *value,
        _ => 0,
    }
}

#[unsafe(no_mangle)]
#[allow(clippy::missing_const_for_fn)]
pub unsafe extern "C" fn mg_value_float_get(value: *const MgValue) -> f64 {
    if value.is_null() {
        return 0.0;
    }
    // SAFETY: handle validity is owned by the Go side contract.
    match unsafe { &(*value).object } {
        MontyObject::Float(value) => *value,
        _ => 0.0,
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_text(
    value: *const MgValue,
    out: *mut MgBytes,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        let object = unsafe { &(*value).object };
        let text = text_for_raw(object);
        write_string(out, &text)
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_bytes_get(
    value: *const MgValue,
    out: *mut MgBytes,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        match unsafe { &(*value).object } {
            MontyObject::Bytes(bytes) => write_bytes(out, bytes),
            _ => Err(ffi_error("TypeError", "value is not bytes")),
        }
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_date_get(
    value: *const MgValue,
    out: *mut MgDate,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "date output pointer is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        let object = unsafe { &(*value).object };
        let MontyObject::Date(date) = object else {
            return Err(ffi_error("TypeError", "value is not date"));
        };
        // SAFETY: out is checked for null above.
        unsafe {
            *out = MgDate {
                year: date.year,
                month: date.month,
                day: date.day,
                _pad: [0; 2],
            };
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_datetime_get(
    value: *const MgValue,
    out: *mut MgDateTime,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "datetime output pointer is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        let object = unsafe { &(*value).object };
        let MontyObject::DateTime(datetime) = object else {
            return Err(ffi_error("TypeError", "value is not datetime"));
        };
        let mut raw = MgDateTime {
            timezone_name: MgBytes {
                ptr: ptr::null_mut(),
                len: 0,
            },
            year: datetime.year,
            microsecond: datetime.microsecond,
            offset_seconds: datetime.offset_seconds.unwrap_or_default(),
            month: datetime.month,
            day: datetime.day,
            hour: datetime.hour,
            minute: datetime.minute,
            second: datetime.second,
            has_offset: u8::from(datetime.offset_seconds.is_some()),
            has_timezone_name: u8::from(datetime.timezone_name.is_some()),
            _pad: 0,
        };
        if let Some(name) = &datetime.timezone_name {
            write_string(ptr::addr_of_mut!(raw.timezone_name), name)?;
        }
        // SAFETY: out is checked for null above.
        unsafe { *out = raw };
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_timedelta_get(
    value: *const MgValue,
    out: *mut MgTimeDelta,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "timedelta output pointer is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        let object = unsafe { &(*value).object };
        let MontyObject::TimeDelta(delta) = object else {
            return Err(ffi_error("TypeError", "value is not timedelta"));
        };
        // SAFETY: out is checked for null above.
        unsafe {
            *out = MgTimeDelta {
                days: delta.days,
                seconds: delta.seconds,
                microseconds: delta.microseconds,
            };
        }
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_timezone_get(
    value: *const MgValue,
    out: *mut MgTimeZone,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "timezone output pointer is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        let object = unsafe { &(*value).object };
        let MontyObject::TimeZone(timezone) = object else {
            return Err(ffi_error("TypeError", "value is not timezone"));
        };
        let mut raw = MgTimeZone {
            name: MgBytes {
                ptr: ptr::null_mut(),
                len: 0,
            },
            offset_seconds: timezone.offset_seconds,
            has_name: u8::from(timezone.name.is_some()),
            _pad: [0; 3],
        };
        if let Some(name) = &timezone.name {
            write_string(ptr::addr_of_mut!(raw.name), name)?;
        }
        // SAFETY: out is checked for null above.
        unsafe { *out = raw };
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_named_tuple_type_name(
    value: *const MgValue,
    out: *mut MgBytes,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        match unsafe { &(*value).object } {
            MontyObject::NamedTuple { type_name, .. } => write_string(out, type_name),
            _ => Err(ffi_error("TypeError", "value is not named tuple")),
        }
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_named_tuple_field_names(
    value: *const MgValue,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        let object = unsafe { &(*value).object };
        let MontyObject::NamedTuple {
            field_names: names, ..
        } = object
        else {
            return Err(ffi_error("TypeError", "value is not named tuple"));
        };
        // SAFETY: out is checked for null above.
        unsafe { *out = string_list_value(names) };
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_dataclass_name(
    value: *const MgValue,
    out: *mut MgBytes,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        match unsafe { &(*value).object } {
            MontyObject::Dataclass { name, .. } => write_string(out, name),
            _ => Err(ffi_error("TypeError", "value is not dataclass")),
        }
    })
}

#[unsafe(no_mangle)]
pub const unsafe extern "C" fn mg_value_dataclass_type_id(value: *const MgValue) -> u64 {
    if value.is_null() {
        return 0;
    }
    // SAFETY: handle validity is owned by the Go side contract.
    match unsafe { &(*value).object } {
        MontyObject::Dataclass { type_id, .. } => *type_id,
        _ => 0,
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_dataclass_field_names(
    value: *const MgValue,
    out: *mut *mut MgValue,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "value output pointer is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        let object = unsafe { &(*value).object };
        let MontyObject::Dataclass {
            field_names: names, ..
        } = object
        else {
            return Err(ffi_error("TypeError", "value is not dataclass"));
        };
        // SAFETY: out is checked for null above.
        unsafe { *out = string_list_value(names) };
        Ok(())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_dataclass_frozen(value: *const MgValue) -> u8 {
    if value.is_null() {
        return 0;
    }
    // SAFETY: handle validity is owned by the Go side contract.
    match unsafe { &(*value).object } {
        MontyObject::Dataclass { frozen, .. } => u8::from(*frozen),
        _ => 0,
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_len(value: *const MgValue) -> usize {
    if value.is_null() {
        return 0;
    }
    // SAFETY: handle validity is owned by the Go side contract.
    match unsafe { &(*value).object } {
        MontyObject::List(values)
        | MontyObject::Tuple(values)
        | MontyObject::Set(values)
        | MontyObject::FrozenSet(values)
        | MontyObject::NamedTuple { values, .. } => values.len(),
        MontyObject::Dict(values) => values.len(),
        MontyObject::Dataclass { attrs, .. } => attrs.len(),
        _ => 0,
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_items_raw(
    value: *const MgValue,
    out: *mut MgRawValue,
    len: usize,
) -> i32 {
    guard(ptr::null_mut(), || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        match unsafe { &(*value).object } {
            MontyObject::List(values)
            | MontyObject::Tuple(values)
            | MontyObject::Set(values)
            | MontyObject::FrozenSet(values)
            | MontyObject::NamedTuple { values, .. } => {
                write_raw_values(out, len, values, "sequence item")
            }
            _ => Err(ffi_error("TypeError", "value is not a sequence")),
        }
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_value_pairs_raw(
    value: *const MgValue,
    out: *mut MgRawPair,
    len: usize,
) -> i32 {
    guard(ptr::null_mut(), || {
        if value.is_null() {
            return Err(ffi_error("TypeError", "value handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        match unsafe { &(*value).object } {
            MontyObject::Dict(values) => write_dict_pairs_raw(out, len, values, "dict pair"),
            MontyObject::Dataclass { attrs, .. } => {
                write_dict_pairs_raw(out, len, attrs, "dataclass pair")
            }
            _ => Err(ffi_error("TypeError", "value is not a dict")),
        }
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_progress_free(progress: *mut MgProgress) {
    if !progress.is_null() {
        // SAFETY: progress handles are allocated with Box::into_raw in this crate.
        unsafe { drop(Box::from_raw(progress)) };
    }
}

unsafe fn write_function_snapshot<T: ResourceTracker>(
    out: *mut MgProgressSnapshot,
    call: &FunctionCall<T>,
) -> Result<(), MgError> {
    // SAFETY: caller provides a non-null output pointer.
    unsafe {
        write_string(ptr::addr_of_mut!((*out).name), &call.function_name)?;
        write_raw_value(
            ptr::addr_of_mut!((*out).args),
            MontyObject::List(call.args.clone()),
        )?;
        write_raw_value(
            ptr::addr_of_mut!((*out).kwargs),
            MontyObject::Dict(call.kwargs.clone().into()),
        )?;
        (*out).call_id = call.call_id;
        (*out).method_call = u8::from(call.method_call);
    }
    Ok(())
}

unsafe fn write_os_snapshot<T: ResourceTracker>(
    out: *mut MgProgressSnapshot,
    call: &OsCall<T>,
) -> Result<(), MgError> {
    // SAFETY: caller provides a non-null output pointer.
    unsafe {
        write_string(ptr::addr_of_mut!((*out).name), &call.function.to_string())?;
        write_raw_value(
            ptr::addr_of_mut!((*out).args),
            MontyObject::List(call.args.clone()),
        )?;
        write_raw_value(
            ptr::addr_of_mut!((*out).kwargs),
            MontyObject::Dict(call.kwargs.clone().into()),
        )?;
        (*out).call_id = call.call_id;
    }
    Ok(())
}

unsafe fn write_name_lookup_snapshot<T: ResourceTracker>(
    out: *mut MgProgressSnapshot,
    lookup: &NameLookup<T>,
) -> Result<(), MgError> {
    // SAFETY: caller provides a non-null output pointer.
    unsafe { write_string(ptr::addr_of_mut!((*out).name), &lookup.name) }
}

unsafe fn init_progress_snapshot(out: *mut MgProgressSnapshot, progress: &MgProgress) {
    let empty = MgBytes {
        ptr: ptr::null_mut(),
        len: 0,
    };
    // SAFETY: caller provides a non-null output pointer.
    unsafe {
        *out = MgProgressSnapshot {
            kind: progress_kind(progress),
            name: empty,
            args: raw_value(KIND_INVALID),
            kwargs: raw_value(KIND_INVALID),
            value: raw_value(KIND_INVALID),
            call_id: 0,
            method_call: 0,
            _pad: [0; 3],
            error: ptr::null_mut(),
        };
    }
}

unsafe fn write_progress_snapshot_ref(
    out: *mut MgProgressSnapshot,
    progress: &MgProgress,
) -> Result<(), MgError> {
    // SAFETY: out is supplied by the caller and must point to writable snapshot storage.
    unsafe { init_progress_snapshot(out, progress) };
    match progress {
        MgProgress::NoLimit(RunProgress::Complete(value))
        | MgProgress::Limited(RunProgress::Complete(value)) => {
            // SAFETY: init_progress_snapshot initialized out.value, and out is writable.
            unsafe { write_raw_value(ptr::addr_of_mut!((*out).value), value.clone()) }
        }
        MgProgress::NoLimit(RunProgress::FunctionCall(call)) => {
            // SAFETY: out points to writable snapshot storage for this call.
            unsafe { write_function_snapshot(out, call) }
        }
        MgProgress::Limited(RunProgress::FunctionCall(call)) => {
            // SAFETY: out points to writable snapshot storage for this call.
            unsafe { write_function_snapshot(out, call) }
        }
        MgProgress::NoLimit(RunProgress::OsCall(call)) => {
            // SAFETY: out points to writable snapshot storage for this call.
            unsafe { write_os_snapshot(out, call) }
        }
        MgProgress::Limited(RunProgress::OsCall(call)) => {
            // SAFETY: out points to writable snapshot storage for this call.
            unsafe { write_os_snapshot(out, call) }
        }
        MgProgress::NoLimit(RunProgress::NameLookup(lookup)) => {
            // SAFETY: out points to writable snapshot storage for this call.
            unsafe { write_name_lookup_snapshot(out, lookup) }
        }
        MgProgress::Limited(RunProgress::NameLookup(lookup)) => {
            // SAFETY: out points to writable snapshot storage for this call.
            unsafe { write_name_lookup_snapshot(out, lookup) }
        }
        MgProgress::NoLimit(RunProgress::ResolveFutures(_))
        | MgProgress::Limited(RunProgress::ResolveFutures(_)) => Ok(()),
    }
}

unsafe fn write_progress_snapshot_output(
    out: *mut MgProgressSnapshotOutput,
    progress: MgProgress,
    stdout: String,
) -> Result<(), MgError> {
    if out.is_null() {
        return Err(ffi_error(
            "TypeError",
            "progress snapshot output pointer is null",
        ));
    }
    // SAFETY: out is checked for null above.
    unsafe {
        write_owned_string(ptr::addr_of_mut!((*out).print), stdout)?;
        write_progress_snapshot_ref(ptr::addr_of_mut!((*out).snapshot), &progress)?;
        (*out).progress = if progress_kind(&progress) == PROGRESS_COMPLETE {
            ptr::null_mut()
        } else {
            Box::into_raw(Box::new(progress))
        };
    }
    Ok(())
}

fn progress_snapshot_output_error(out: *mut MgProgressSnapshotOutput) -> *mut *mut MgError {
    if out.is_null() {
        return ptr::null_mut();
    }
    // SAFETY: out is non-null and points to a caller-provided output struct.
    unsafe { ptr::addr_of_mut!((*out).error) }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_progress_snapshot(
    progress: *const MgProgress,
    out: *mut MgProgressSnapshot,
) -> i32 {
    let out_error = if out.is_null() {
        ptr::null_mut()
    } else {
        // SAFETY: out is non-null and points to a caller-provided output struct.
        unsafe { ptr::addr_of_mut!((*out).error) }
    };
    guard(out_error, || {
        if progress.is_null() {
            return Err(ffi_error("TypeError", "progress handle is null"));
        }
        if out.is_null() {
            return Err(ffi_error(
                "TypeError",
                "progress snapshot output pointer is null",
            ));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        unsafe { write_progress_snapshot_ref(out, &*progress) }
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_progress_pending_len(progress: *const MgProgress) -> usize {
    with_resolve_futures(progress, <[u32]>::len).unwrap_or(0)
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_progress_pending_id(progress: *const MgProgress, index: usize) -> u32 {
    with_resolve_futures(progress, |ids| ids.get(index).copied())
        .flatten()
        .unwrap_or(0)
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_progress_resume_pending(
    progress: *mut MgProgress,
    out: *mut MgProgressOutput,
) -> i32 {
    guard(progress_output_error(out), || {
        if progress.is_null() || out.is_null() {
            return Err(ffi_error(
                "TypeError",
                "progress handle or output pointer is null",
            ));
        }
        // SAFETY: progress is consumed exactly once by this call.
        let progress = unsafe { *Box::from_raw(progress) };
        let (next, stdout) = resume_pending(progress)?;
        write_progress_output(out, next, stdout)
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_progress_resume_return_raw(
    progress: *mut MgProgress,
    value: *mut MgRawValue,
    out: *mut MgProgressOutput,
) -> i32 {
    guard(progress_output_error(out), || {
        if progress.is_null() || out.is_null() {
            return Err(ffi_error(
                "TypeError",
                "progress handle or output pointer is null",
            ));
        }
        if value.is_null() {
            return Err(ffi_error("TypeError", "raw value pointer is null"));
        }
        // SAFETY: progress is consumed exactly once by this call.
        let progress = unsafe { *Box::from_raw(progress) };
        // SAFETY: value is checked for null above and only read during this call.
        let value = read_raw_value(unsafe { &mut *value })?;
        let (next, stdout) = resume_progress(progress, ExtFunctionResult::Return(value))?;
        write_progress_output(out, next, stdout)
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_progress_resume_return_raw_snapshot(
    progress: *mut MgProgress,
    value: *mut MgRawValue,
    out: *mut MgProgressSnapshotOutput,
) -> i32 {
    guard(progress_snapshot_output_error(out), || {
        if progress.is_null() || out.is_null() {
            return Err(ffi_error(
                "TypeError",
                "progress handle or output pointer is null",
            ));
        }
        if value.is_null() {
            return Err(ffi_error("TypeError", "raw value pointer is null"));
        }
        // SAFETY: progress is consumed exactly once by this call.
        let progress = unsafe { *Box::from_raw(progress) };
        // SAFETY: value is checked for null above and only read during this call.
        let value = read_raw_value(unsafe { &mut *value })?;
        let (next, stdout) = resume_progress(progress, ExtFunctionResult::Return(value))?;
        // SAFETY: out is checked for null above.
        unsafe { write_progress_snapshot_output(out, next, stdout) }
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_progress_resume_exception(
    progress: *mut MgProgress,
    exc_type_ptr: *const u8,
    exc_type_len: usize,
    message_ptr: *const u8,
    message_len: usize,
    out: *mut MgProgressOutput,
) -> i32 {
    guard(progress_output_error(out), || {
        if progress.is_null() || out.is_null() {
            return Err(ffi_error(
                "TypeError",
                "progress handle or output pointer is null",
            ));
        }
        let exception = exception_from_raw(
            MgStr {
                ptr: exc_type_ptr,
                len: exc_type_len,
            },
            MgStr {
                ptr: message_ptr,
                len: message_len,
            },
        )?;
        // SAFETY: progress is consumed exactly once by this call.
        let progress = unsafe { *Box::from_raw(progress) };
        let (next, stdout) = resume_progress(progress, ExtFunctionResult::Error(exception))?;
        write_progress_output(out, next, stdout)
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_progress_resume_name_value_raw(
    progress: *mut MgProgress,
    value: *mut MgRawValue,
    out: *mut MgProgressOutput,
) -> i32 {
    guard(progress_output_error(out), || {
        if progress.is_null() || out.is_null() {
            return Err(ffi_error(
                "TypeError",
                "progress handle or output pointer is null",
            ));
        }
        if value.is_null() {
            return Err(ffi_error("TypeError", "raw value pointer is null"));
        }
        // SAFETY: progress is consumed exactly once by this call.
        let progress = unsafe { *Box::from_raw(progress) };
        // SAFETY: value is checked for null above and only read during this call.
        let value = read_raw_value(unsafe { &mut *value })?;
        let (next, stdout) = resume_name(progress, NameLookupResult::Value(value))?;
        write_progress_output(out, next, stdout)
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_progress_resume_name_value_raw_snapshot(
    progress: *mut MgProgress,
    value: *mut MgRawValue,
    out: *mut MgProgressSnapshotOutput,
) -> i32 {
    guard(progress_snapshot_output_error(out), || {
        if progress.is_null() || out.is_null() {
            return Err(ffi_error(
                "TypeError",
                "progress handle or output pointer is null",
            ));
        }
        if value.is_null() {
            return Err(ffi_error("TypeError", "raw value pointer is null"));
        }
        // SAFETY: progress is consumed exactly once by this call.
        let progress = unsafe { *Box::from_raw(progress) };
        // SAFETY: value is checked for null above and only read during this call.
        let value = read_raw_value(unsafe { &mut *value })?;
        let (next, stdout) = resume_name(progress, NameLookupResult::Value(value))?;
        // SAFETY: out is checked for null above.
        unsafe { write_progress_snapshot_output(out, next, stdout) }
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_progress_resume_name_undefined(
    progress: *mut MgProgress,
    out: *mut MgProgressOutput,
) -> i32 {
    guard(progress_output_error(out), || {
        if progress.is_null() || out.is_null() {
            return Err(ffi_error(
                "TypeError",
                "progress handle or output pointer is null",
            ));
        }
        // SAFETY: progress is consumed exactly once by this call.
        let progress = unsafe { *Box::from_raw(progress) };
        let (next, stdout) = resume_name(progress, NameLookupResult::Undefined)?;
        write_progress_output(out, next, stdout)
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_progress_resume_name_undefined_snapshot(
    progress: *mut MgProgress,
    out: *mut MgProgressSnapshotOutput,
) -> i32 {
    guard(progress_snapshot_output_error(out), || {
        if progress.is_null() || out.is_null() {
            return Err(ffi_error(
                "TypeError",
                "progress handle or output pointer is null",
            ));
        }
        // SAFETY: progress is consumed exactly once by this call.
        let progress = unsafe { *Box::from_raw(progress) };
        let (next, stdout) = resume_name(progress, NameLookupResult::Undefined)?;
        // SAFETY: out is checked for null above.
        unsafe { write_progress_snapshot_output(out, next, stdout) }
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_progress_resume_futures(
    progress: *mut MgProgress,
    results: *mut MgFutureResult,
    results_len: usize,
    out: *mut MgProgressOutput,
) -> i32 {
    guard(progress_output_error(out), || {
        if progress.is_null() || out.is_null() {
            return Err(ffi_error(
                "TypeError",
                "progress handle or output pointer is null",
            ));
        }
        // SAFETY: progress is consumed exactly once by this call.
        let progress = unsafe { *Box::from_raw(progress) };
        let results = read_future_results(results, results_len)?;
        let (next, stdout) = resume_futures(progress, results)?;
        write_progress_output(out, next, stdout)
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_progress_dump(
    progress: *const MgProgress,
    out: *mut MgBytes,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if progress.is_null() {
            return Err(ffi_error("TypeError", "progress handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        let bytes = postcard::to_allocvec(unsafe { &*progress })
            .map_err(|err| ffi_error("SerializationError", err.to_string()))?;
        write_bytes(out, &bytes)
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_progress_load(
    ptr: *const u8,
    len: usize,
    out_progress: *mut *mut MgProgress,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if out_progress.is_null() {
            return Err(ffi_error("TypeError", "progress output pointer is null"));
        }
        let bytes = as_bytes(ptr, len)?;
        let progress: MgProgress = postcard::from_bytes(bytes)
            .map_err(|err| ffi_error("SerializationError", err.to_string()))?;
        // SAFETY: out_progress is checked for null above.
        unsafe { *out_progress = Box::into_raw(Box::new(progress)) };
        Ok(())
    })
}
