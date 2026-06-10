//! Program compilation and execution entry points.

use std::ptr;

use monty::{
    ExtFunctionResult, JsonMontyObject, MontyException, MontyObject, MontyRun, NameLookupResult,
    NoLimitTracker, ResourceTracker, RunProgress,
};

use crate::mount::{dispatch_mount_os_call, mount_slots};
use crate::progress::{StepFailure, StepOutcome, write_step_outcome};
use crate::{
    FAST_FORMAT_FLAT, FAST_FORMAT_RAW, FAST_SCRATCH_CAP, HOST_CALL_FUNCTION, HOST_CALL_OS,
    HOST_CALLBACK_EXCEPTION, HOST_CALLBACK_NOT_HANDLED, HOST_CALLBACK_RETURN, KIND_BYTES,
    KIND_DICT, KIND_INVALID, KIND_LIST, LimitSpec, MgBytes, MgCompileRunFastRawArgs, MgError,
    MgHostFunctionOutput, MgLimits, MgProgram, MgProgramCompileArgs, MgProgress,
    MgProgressSnapshotOutput, MgRawPair, MgRawValue, MgRunFastOutput, MgRunHostArgs,
    MgRunJsonOutput, MgStr, PrintBuf, STATUS_ERR, STATUS_OK, StoredProgram, as_bytes, as_str,
    exception_from_raw, ffi_error, free_raw_value, from_monty_error, guard, limits_from_raw,
    raw_value, read_borrowed_str_list, read_raw_value, read_raw_values, read_string_list,
    string_list_value, write_flat_value, write_owned_bytes, write_raw_value,
};

// ---------------------------------------------------------------------------
// Run helpers
// ---------------------------------------------------------------------------

fn run_to_value(
    runner: &MontyRun,
    inputs: Vec<MontyObject>,
    spec: LimitSpec,
    print: &mut PrintBuf,
) -> Result<MontyObject, MgError> {
    if spec.tracked() {
        runner.run(inputs, spec.tracker(), print.writer())
    } else {
        runner.run(inputs, NoLimitTracker, print.writer())
    }
    .map_err(from_monty_error)
}

/// Starts iterative execution, mapping the initial state (paused, complete,
/// or raised) onto the shared step-outcome shape.
fn start_outcome(
    runner: &MontyRun,
    inputs: Vec<MontyObject>,
    spec: LimitSpec,
    print: &mut PrintBuf,
) -> Result<StepOutcome, StepFailure> {
    let result = if spec.tracked() {
        runner
            .clone()
            .start(inputs, spec.tracker(), print.writer())
            .map(|progress| match progress {
                RunProgress::Complete(value) => StepOutcome::Done { value, repl: None },
                other => StepOutcome::Paused(Box::new(MgProgress::Limited(other))),
            })
    } else {
        runner
            .clone()
            .start(inputs, NoLimitTracker, print.writer())
            .map(|progress| match progress {
                RunProgress::Complete(value) => StepOutcome::Done { value, repl: None },
                other => StepOutcome::Paused(Box::new(MgProgress::NoLimit(other))),
            })
    };
    result.map_err(|exc| StepFailure::from(from_monty_error(exc)))
}

// ---------------------------------------------------------------------------
// Host-dispatch loop
// ---------------------------------------------------------------------------

pub enum CallbackVerdict {
    Return(MontyObject),
    Error(MontyException),
    NotHandled,
}

type HostCallbackFn = unsafe extern "C" fn(
    usize,
    u32,
    *const u8,
    usize,
    *const MgRawValue,
    *const MgRawValue,
    *mut MgHostFunctionOutput,
) -> i32;

/// Invokes the Go host callback for one pending call.
///
/// Fast path: when every positional and keyword value is a scalar, encode
/// borrowed views over the call's own storage — no `MontyObject` clones and
/// no owned raw trees to free afterwards.
pub fn call_host_callback(
    kind: u32,
    name: &str,
    call_args: &[MontyObject],
    call_kwargs: &[(MontyObject, MontyObject)],
    callback: HostCallbackFn,
    user_data: usize,
) -> Result<CallbackVerdict, MgError> {
    if let Some((mut arg_items, mut kwarg_pairs)) = borrowed_call_views(call_args, call_kwargs) {
        let mut args = raw_value(KIND_LIST);
        if !arg_items.is_empty() {
            args.ptr = arg_items.as_mut_ptr().cast();
            args.len = arg_items.len();
        }
        let mut kwargs = raw_value(KIND_DICT);
        if !kwarg_pairs.is_empty() {
            kwargs.ptr = kwarg_pairs.as_mut_ptr().cast();
            kwargs.len = kwarg_pairs.len();
        }
        return invoke_host_callback(kind, name, callback, user_data, &args, &kwargs);
    }
    let mut args = crate::raw_sequence(KIND_LIST, call_args.to_vec())?;
    let mut kwargs = crate::raw_dict(KIND_DICT, call_kwargs.to_vec().into())?;
    let result = invoke_host_callback(kind, name, callback, user_data, &args, &kwargs);
    free_raw_value(&mut args);
    free_raw_value(&mut kwargs);
    result
}

fn invoke_host_callback(
    kind: u32,
    name: &str,
    callback: HostCallbackFn,
    user_data: usize,
    args: &MgRawValue,
    kwargs: &MgRawValue,
) -> Result<CallbackVerdict, MgError> {
    let mut out = MgHostFunctionOutput {
        value: raw_value(KIND_INVALID),
        exc_type: MgStr::empty(),
        message: MgStr::empty(),
    };
    // SAFETY: callback is provided by Go for this FFI call, and all pointers
    // passed here reference stack values or call-owned Monty strings.
    let status = unsafe {
        callback(
            user_data,
            kind,
            name.as_ptr(),
            name.len(),
            ptr::from_ref(args),
            ptr::from_ref(kwargs),
            ptr::addr_of_mut!(out),
        )
    };
    match status {
        HOST_CALLBACK_RETURN => read_raw_value(&mut out.value).map(CallbackVerdict::Return),
        HOST_CALLBACK_EXCEPTION => {
            exception_from_raw(out.exc_type, out.message).map(CallbackVerdict::Error)
        }
        HOST_CALLBACK_NOT_HANDLED => Ok(CallbackVerdict::NotHandled),
        _ => Err(ffi_error("RuntimeError", "host function callback failed")),
    }
}

/// Borrowed raw views of every positional and keyword argument, or `None`
/// when any value is a non-scalar that needs the owned (cloning) encoding.
fn borrowed_call_views(
    call_args: &[MontyObject],
    call_kwargs: &[(MontyObject, MontyObject)],
) -> Option<(Vec<MgRawValue>, Vec<MgRawPair>)> {
    let mut args = Vec::with_capacity(call_args.len());
    for arg in call_args {
        args.push(borrowed_raw_value(arg)?);
    }
    let mut kwargs = Vec::with_capacity(call_kwargs.len());
    for pair in call_kwargs {
        kwargs.push(MgRawPair {
            key: borrowed_raw_value(&pair.0)?,
            value: borrowed_raw_value(&pair.1)?,
        });
    }
    Some((args, kwargs))
}

fn borrowed_raw_value(object: &MontyObject) -> Option<MgRawValue> {
    match object {
        MontyObject::Ellipsis => Some(raw_value(crate::KIND_ELLIPSIS)),
        MontyObject::None => Some(raw_value(crate::KIND_NONE)),
        MontyObject::Bool(value) => {
            let mut raw = raw_value(crate::KIND_BOOL);
            raw.bool_value = u8::from(*value);
            Some(raw)
        }
        MontyObject::Int(value) => {
            let mut raw = raw_value(crate::KIND_INT);
            raw.int_value = *value;
            Some(raw)
        }
        MontyObject::Float(value) => {
            let mut raw = raw_value(crate::KIND_FLOAT);
            raw.float_value = *value;
            Some(raw)
        }
        MontyObject::String(value) => {
            let mut raw = raw_value(crate::KIND_STRING);
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

pub struct HostDispatch<'a> {
    pub(crate) host_names: &'a [&'a str],
    pub(crate) slots: Vec<std::sync::Arc<std::sync::Mutex<Option<monty::fs::Mount>>>>,
    pub(crate) callback: Option<HostCallbackFn>,
    pub(crate) user_data: usize,
}

impl HostDispatch<'_> {
    pub(crate) fn from_host_args(
        args: &MgRunHostArgs,
    ) -> Result<(Self, Vec<&'static str>), MgError> {
        let names = read_borrowed_str_list(args.host_names, args.host_name_count)?;
        let slots = mount_slots(args.mounts, args.mount_count)?;
        Ok((
            Self {
                host_names: &[],
                slots,
                callback: args.callback,
                user_data: args.callback_data,
            },
            names,
        ))
    }

    pub(crate) fn from_feed_args(
        args: &crate::MgReplFeedArgs,
    ) -> Result<(Self, Vec<&'static str>), MgError> {
        let names = read_borrowed_str_list(args.host_names, args.host_name_count)?;
        let slots = mount_slots(args.mounts, args.mount_count)?;
        Ok((
            Self {
                host_names: &[],
                slots,
                callback: args.callback,
                user_data: args.callback_data,
            },
            names,
        ))
    }

    pub(crate) fn resolve_name(&self, name: &str) -> NameLookupResult {
        if self.host_names.contains(&name) {
            NameLookupResult::Value(MontyObject::Function {
                name: name.to_owned(),
                docstring: None,
            })
        } else {
            NameLookupResult::Undefined
        }
    }

    /// Resolves a pending external function call through the callback.
    pub(crate) fn function_result(
        &self,
        name: &str,
        args: &[MontyObject],
        kwargs: &[(MontyObject, MontyObject)],
    ) -> Result<ExtFunctionResult, MgError> {
        let callback = self
            .callback
            .ok_or_else(|| ffi_error("TypeError", "host function callback is null"))?;
        match call_host_callback(
            HOST_CALL_FUNCTION,
            name,
            args,
            kwargs,
            callback,
            self.user_data,
        )? {
            CallbackVerdict::Return(value) => Ok(ExtFunctionResult::Return(value)),
            CallbackVerdict::Error(exc) => Ok(ExtFunctionResult::Error(exc)),
            CallbackVerdict::NotHandled => Err(ffi_error(
                "RuntimeError",
                format!("host function {name:?} reported not-handled"),
            )),
        }
    }

    /// Resolves a pending OS call: mount table first, then the callback, then
    /// Monty's default unhandled behavior.
    pub(crate) fn os_result(
        &self,
        function: monty::OsFunction,
        args: &[MontyObject],
        kwargs: &[(MontyObject, MontyObject)],
    ) -> Result<ExtFunctionResult, MgError> {
        match dispatch_mount_os_call(&self.slots, function, args, kwargs) {
            Ok(Some(value)) => return Ok(ExtFunctionResult::Return(value)),
            Ok(None) => {}
            // Mount-level failures carry the Python exception to raise.
            Err(error) => match error.exc {
                Some(exc) => return Ok(ExtFunctionResult::Error(*exc)),
                None => return Err(error),
            },
        }
        if let Some(callback) = self.callback {
            let name = function.to_string();
            match call_host_callback(HOST_CALL_OS, &name, args, kwargs, callback, self.user_data)? {
                CallbackVerdict::Return(value) => return Ok(ExtFunctionResult::Return(value)),
                CallbackVerdict::Error(exc) => return Ok(ExtFunctionResult::Error(exc)),
                CallbackVerdict::NotHandled => {}
            }
        }
        Ok(ExtFunctionResult::Error(function.on_no_handler(args)))
    }
}

fn run_host_loop<T: ResourceTracker>(
    mut progress: RunProgress<T>,
    dispatch: &HostDispatch<'_>,
    print: &mut PrintBuf,
) -> Result<MontyObject, MgError> {
    loop {
        progress = match progress {
            RunProgress::Complete(value) => return Ok(value),
            RunProgress::NameLookup(lookup) => {
                let result = dispatch.resolve_name(&lookup.name);
                lookup
                    .resume(result, print.writer())
                    .map_err(from_monty_error)?
            }
            RunProgress::FunctionCall(call) => {
                let result =
                    dispatch.function_result(&call.function_name, &call.args, &call.kwargs)?;
                call.resume(result, print.writer())
                    .map_err(from_monty_error)?
            }
            RunProgress::OsCall(call) => {
                let result = dispatch.os_result(call.function, &call.args, &call.kwargs)?;
                call.resume(result, print.writer())
                    .map_err(from_monty_error)?
            }
            RunProgress::ResolveFutures(_) => {
                return Err(ffi_error(
                    "RuntimeError",
                    "host callback run path does not support pending futures; use start/resume",
                ));
            }
        };
    }
}

// ---------------------------------------------------------------------------
// Fast output encoding
// ---------------------------------------------------------------------------

/// Populate an `MgRunFastOutput` with the result of a run. Scalar values are
/// returned via `FAST_FORMAT_RAW` (no bytes allocation) so the Go side can
/// decode without freeing a Rust-owned buffer. Compound values fall through
/// to the flat byte stream when it can be produced, and to the owned-handle
/// `MgRawValue` otherwise.
///
/// # Safety
/// `out` must point to a writable `MgRunFastOutput`.
pub unsafe fn write_fast_output(
    out: *mut MgRunFastOutput,
    value: MontyObject,
    print: &mut PrintBuf,
) -> Result<(), MgError> {
    // SAFETY: out is non-null by this function's safety contract and is only
    // written during this call.
    unsafe {
        (*out).format = FAST_FORMAT_RAW;
        (*out).bytes_in_scratch = 0;
        (*out).value = raw_value(KIND_INVALID);
        (*out).bytes = MgBytes::empty();
        print.finish(
            ptr::addr_of_mut!((*out).print),
            ptr::addr_of_mut!((*out).print_flags),
        )?;
    }
    if let Some(raw) = fast_scalar_raw(&value) {
        // SAFETY: out is non-null by contract.
        unsafe { (*out).value = raw };
        return Ok(());
    }
    // Pre-size to the scratch capacity: one allocation covers every payload
    // that will land in scratch, instead of a doubling realloc chain.
    let mut bytes = Vec::with_capacity(FAST_SCRATCH_CAP);
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
        MontyObject::Ellipsis => Some(raw_value(crate::KIND_ELLIPSIS)),
        MontyObject::None => Some(raw_value(crate::KIND_NONE)),
        MontyObject::Bool(value) => {
            let mut raw = raw_value(crate::KIND_BOOL);
            raw.bool_value = u8::from(*value);
            Some(raw)
        }
        MontyObject::Int(value) => {
            let mut raw = raw_value(crate::KIND_INT);
            raw.int_value = *value;
            Some(raw)
        }
        MontyObject::Float(value) => {
            let mut raw = raw_value(crate::KIND_FLOAT);
            raw.float_value = *value;
            Some(raw)
        }
        _ => None,
    }
}

const fn fast_output_error(out: *mut MgRunFastOutput) -> *mut *mut MgError {
    if out.is_null() {
        return ptr::null_mut();
    }
    // SAFETY: out is non-null and points to a caller-provided output struct.
    unsafe { ptr::addr_of_mut!((*out).error) }
}

// ---------------------------------------------------------------------------
// Entry points
// ---------------------------------------------------------------------------

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
        let names = read_string_list(args.input_names, args.input_count)?;
        let runner = MontyRun::new(code, &script_name, names.clone()).map_err(from_monty_error)?;
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
        write_owned_bytes(out, bytes)
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
        crate::write_string(out, unsafe { (*program).runner.code() })
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
        crate::write_string(out, unsafe { &(*program).script_name })
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_program_input_names(
    program: *const MgProgram,
    out_names: *mut *mut crate::MgValue,
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
pub unsafe extern "C" fn mg_program_run_fast_raw(
    program: *const MgProgram,
    inputs: *mut MgRawValue,
    input_count: usize,
    limits: *const MgLimits,
    out: *mut MgRunFastOutput,
) -> i32 {
    guard(fast_output_error(out), || {
        if program.is_null() {
            return Err(ffi_error("TypeError", "program handle is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "fast value output pointer is null"));
        }
        let input_values = read_raw_values(inputs, input_count)?;
        let mut print = PrintBuf::new();
        // SAFETY: handle validity is owned by the Go side contract.
        let value = run_to_value(
            unsafe { &(*program).runner },
            input_values,
            limits_from_raw(limits),
            &mut print,
        )?;
        // SAFETY: out is checked for null above.
        unsafe { write_fast_output(out, value, &mut print) }
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
    guard(fast_output_error(out), || {
        if args.is_null() {
            return Err(ffi_error("TypeError", "compile-run args pointer is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "fast value output pointer is null"));
        }
        // SAFETY: args was checked for null and is only read during this call.
        let args = unsafe { &*args };
        let code = as_str(args.code)?.to_owned();
        let script_name = as_str(args.script_name)?;
        let names = read_string_list(args.input_names, args.input_count)?;
        // The program never outlives this call, so no MgProgram (and no clone
        // of names or script_name) is materialized.
        let runner = MontyRun::new(code, script_name, names).map_err(from_monty_error)?;
        let input_values = read_raw_values(args.input_values, args.input_value_count)?;
        let mut print = PrintBuf::new();
        let value = run_to_value(
            &runner,
            input_values,
            limits_from_raw(args.limits),
            &mut print,
        )?;
        // SAFETY: out was checked for null above.
        unsafe { write_fast_output(out, value, &mut print) }
    })
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
        let mut print = PrintBuf::new();
        // SAFETY: handle validity is owned by the Go side contract.
        let value = run_to_value(
            unsafe { &(*program).runner },
            input_values,
            limits_from_raw(limits),
            &mut print,
        )?;
        let json = serde_json::to_vec(&JsonMontyObject(&value))
            .map_err(|err| ffi_error("SerializationError", err.to_string()))?;
        // SAFETY: out is checked for null above.
        unsafe {
            print.finish(
                ptr::addr_of_mut!((*out).print),
                ptr::addr_of_mut!((*out).print_flags),
            )?;
            write_owned_bytes(ptr::addr_of_mut!((*out).value), json)
        }
    })
}

/// Runs to completion in one hop, dispatching external functions, mounts, and
/// OS calls through the Go callback. Output print streams live through
/// `args.print` when provided.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_program_run_host_raw(
    program: *const MgProgram,
    args: *const MgRunHostArgs,
    out: *mut MgRunFastOutput,
) -> i32 {
    guard(fast_output_error(out), || {
        if program.is_null() {
            return Err(ffi_error("TypeError", "program handle is null"));
        }
        if args.is_null() {
            return Err(ffi_error("TypeError", "host run args pointer is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "fast value output pointer is null"));
        }
        // SAFETY: args is checked for null above and only read during this call.
        let args = unsafe { &*args };
        let input_values = read_raw_values(args.inputs, args.input_count)?;
        let (mut dispatch, names) = HostDispatch::from_host_args(args)?;
        dispatch.host_names = &names;
        let mut print = PrintBuf::for_callback(args.print, args.print_data);
        let spec = limits_from_raw(args.limits);
        // SAFETY: handle validity is owned by the Go side contract.
        let runner = unsafe { &(*program).runner };
        let value = if spec.tracked() {
            let progress = runner
                .clone()
                .start(input_values, spec.tracker(), print.writer())
                .map_err(from_monty_error)?;
            run_host_loop(progress, &dispatch, &mut print)?
        } else {
            let progress = runner
                .clone()
                .start(input_values, NoLimitTracker, print.writer())
                .map_err(from_monty_error)?;
            run_host_loop(progress, &dispatch, &mut print)?
        };
        // SAFETY: out is checked for null above.
        unsafe { write_fast_output(out, value, &mut print) }
    })
}

/// Starts iterative execution and writes the first user-visible state.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_program_start_raw_snapshot(
    program: *const MgProgram,
    inputs: *mut MgRawValue,
    input_count: usize,
    limits: *const MgLimits,
    out: *mut MgProgressSnapshotOutput,
) -> i32 {
    let out_error = if out.is_null() {
        ptr::null_mut()
    } else {
        // SAFETY: out is non-null and points to a caller-provided output struct.
        unsafe { ptr::addr_of_mut!((*out).error) }
    };
    let mut failed = false;
    let status = guard(out_error, || {
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
        let mut print = PrintBuf::new();
        // SAFETY: handle validity is owned by the Go side contract.
        let outcome = start_outcome(
            unsafe { &(*program).runner },
            input_values,
            limits_from_raw(limits),
            &mut print,
        );
        // SAFETY: out is checked for null above.
        failed = unsafe { write_step_outcome(out, outcome, &mut print) }?;
        Ok(())
    });
    if status != STATUS_OK {
        return status;
    }
    if failed { STATUS_ERR } else { STATUS_OK }
}
