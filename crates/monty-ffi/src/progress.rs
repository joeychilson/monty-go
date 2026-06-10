//! Suspendable execution: snapshots and resume entry points.
//!
//! `MgProgress` covers program and REPL executions, with and without resource
//! tracking. Every resume entry point consumes the progress handle and fills
//! an `MgProgressSnapshotOutput` with the next state in a single hop:
//! - paused again → `progress` holds the next handle, `snapshot` describes it;
//! - complete → `snapshot.kind == PROGRESS_COMPLETE` with the value, and for
//!   REPL executions `repl` returns the updated session;
//! - Python exception → `error` is set, and for REPL executions `repl`
//!   returns the preserved session (`ReplStartError`).

use std::ptr;

use monty::{
    ExtFunctionResult, JsonMontyArray, JsonMontyPairs, MontyObject, NameLookupResult, PrintWriter,
    ReplProgress, ResourceTracker, RunProgress,
};

use crate::{
    FUTURE_RESULT_ERROR, FUTURE_RESULT_NOT_FOUND, FUTURE_RESULT_RETURN, KIND_INVALID, MgBytes,
    MgCancelToken, MgError, MgFutureResult, MgProgress, MgProgressSnapshot,
    MgProgressSnapshotOutput, MgRawValue, MgRepl, MgStr, PRINT_PLAIN, PROGRESS_COMPLETE,
    PROGRESS_FUNCTION_CALL, PROGRESS_NAME_LOOKUP, PROGRESS_OS_CALL, PROGRESS_RESOLVE_FUTURES,
    PrintBuf, ReplInner, STATUS_ERR, STATUS_ERR_RETAINED, STATUS_OK, as_bytes, exception_from_raw,
    ffi_error, free_owned_raw_handle, from_monty_error, guard, raw_value, read_raw_value,
    write_owned_bytes, write_raw_value, write_string,
};

// ---------------------------------------------------------------------------
// Step machinery
// ---------------------------------------------------------------------------

/// One resume request, independent of the progress variant.
pub enum Action {
    Return(ExtFunctionResult),
    Pending,
    NotHandled,
    Name(NameLookupResult),
    Futures(Vec<(u32, ExtFunctionResult)>),
}

impl Action {
    const fn describe(&self) -> &'static str {
        match self {
            Self::Return(_) => "an external result",
            Self::Pending => "a pending future",
            Self::NotHandled => "the unhandled-OS fallback",
            Self::Name(_) => "a name lookup result",
            Self::Futures(_) => "future results",
        }
    }
}

pub enum StepOutcome {
    Paused(Box<MgProgress>),
    Done {
        value: MontyObject,
        repl: Option<Box<ReplInner>>,
    },
}

pub struct StepFailure {
    pub(crate) error: MgError,
    pub(crate) repl: Option<Box<ReplInner>>,
}

impl From<MgError> for StepFailure {
    fn from(error: MgError) -> Self {
        Self { error, repl: None }
    }
}

fn mismatch(action: &Action, state: &'static str) -> MgError {
    ffi_error(
        "RuntimeError",
        format!(
            "progress state ({state}) cannot be resumed with {}",
            action.describe()
        ),
    )
}

fn step_run<T: ResourceTracker>(
    progress: RunProgress<T>,
    action: Action,
    print: PrintWriter<'_>,
    wrap: impl Fn(RunProgress<T>) -> MgProgress,
) -> Result<StepOutcome, StepFailure> {
    let next = match (progress, action) {
        (RunProgress::FunctionCall(call), Action::Return(result)) => call.resume(result, print),
        (RunProgress::FunctionCall(call), Action::Pending) => call.resume_pending(print),
        (RunProgress::OsCall(call), Action::Return(result)) => call.resume(result, print),
        (RunProgress::OsCall(call), Action::NotHandled) => {
            let exc = call.function.on_no_handler(&call.args);
            call.resume(ExtFunctionResult::Error(exc), print)
        }
        (RunProgress::NameLookup(lookup), Action::Name(result)) => lookup.resume(result, print),
        (RunProgress::ResolveFutures(state), Action::Futures(results)) => {
            state.resume(results, print)
        }
        (progress, action) => return Err(mismatch(&action, run_state_name(&progress)).into()),
    }
    .map_err(|exc| StepFailure::from(from_monty_error(exc)))?;
    Ok(match next {
        RunProgress::Complete(value) => StepOutcome::Done { value, repl: None },
        other => StepOutcome::Paused(Box::new(wrap(other))),
    })
}

fn step_repl<T: ResourceTracker>(
    progress: ReplProgress<T>,
    action: Action,
    print: PrintWriter<'_>,
    wrap: impl Fn(ReplProgress<T>) -> MgProgress,
    wrap_repl: impl Fn(monty::MontyRepl<T>) -> ReplInner,
) -> Result<StepOutcome, StepFailure> {
    let result = match (progress, action) {
        (ReplProgress::FunctionCall(call), Action::Return(result)) => call.resume(result, print),
        (ReplProgress::FunctionCall(call), Action::Pending) => call.resume_pending(print),
        (ReplProgress::OsCall(call), Action::Return(result)) => call.resume(result, print),
        (ReplProgress::OsCall(call), Action::NotHandled) => {
            let exc = call.function.on_no_handler(&call.args);
            call.resume(ExtFunctionResult::Error(exc), print)
        }
        (ReplProgress::NameLookup(lookup), Action::Name(result)) => lookup.resume(result, print),
        (ReplProgress::ResolveFutures(state), Action::Futures(results)) => {
            state.resume(results, print)
        }
        // Mismatched action: recover the session so the REPL stays usable.
        (progress, action) => {
            let state = repl_state_name(&progress);
            let repl = repl_progress_into_repl(progress);
            return Err(StepFailure {
                error: mismatch(&action, state),
                repl: Some(Box::new(wrap_repl(repl))),
            });
        }
    };
    match result {
        Ok(ReplProgress::Complete { repl, value }) => Ok(StepOutcome::Done {
            value,
            repl: Some(Box::new(wrap_repl(repl))),
        }),
        Ok(other) => Ok(StepOutcome::Paused(Box::new(wrap(other)))),
        Err(start_error) => Err(StepFailure {
            error: from_monty_error(start_error.error),
            repl: Some(Box::new(wrap_repl(start_error.repl))),
        }),
    }
}

const fn run_state_name<T: ResourceTracker>(progress: &RunProgress<T>) -> &'static str {
    match progress {
        RunProgress::FunctionCall(_) => "function call",
        RunProgress::OsCall(_) => "os call",
        RunProgress::ResolveFutures(_) => "resolve futures",
        RunProgress::NameLookup(_) => "name lookup",
        RunProgress::Complete(_) => "complete",
    }
}

const fn repl_state_name<T: ResourceTracker>(progress: &ReplProgress<T>) -> &'static str {
    match progress {
        ReplProgress::FunctionCall(_) => "function call",
        ReplProgress::OsCall(_) => "os call",
        ReplProgress::ResolveFutures(_) => "resolve futures",
        ReplProgress::NameLookup(_) => "name lookup",
        ReplProgress::Complete { .. } => "complete",
    }
}

fn repl_progress_into_repl<T: ResourceTracker>(progress: ReplProgress<T>) -> monty::MontyRepl<T> {
    match progress {
        ReplProgress::FunctionCall(call) => call.into_repl(),
        ReplProgress::OsCall(call) => call.into_repl(),
        ReplProgress::ResolveFutures(state) => state.into_repl(),
        ReplProgress::NameLookup(lookup) => lookup.into_repl(),
        ReplProgress::Complete { repl, .. } => repl,
    }
}

/// Applies one action to a consumed progress value.
pub fn step_progress(
    progress: MgProgress,
    action: Action,
    print: &mut PrintBuf,
) -> Result<StepOutcome, StepFailure> {
    match progress {
        MgProgress::NoLimit(p) => step_run(p, action, print.writer(), MgProgress::NoLimit),
        MgProgress::Limited(p) => step_run(p, action, print.writer(), MgProgress::Limited),
        MgProgress::ReplNoLimit(p) => step_repl(
            p,
            action,
            print.writer(),
            MgProgress::ReplNoLimit,
            ReplInner::NoLimit,
        ),
        MgProgress::ReplLimited(p) => step_repl(
            p,
            action,
            print.writer(),
            MgProgress::ReplLimited,
            ReplInner::Limited,
        ),
    }
}

// ---------------------------------------------------------------------------
// Snapshot writers
// ---------------------------------------------------------------------------

const fn run_progress_kind<T: ResourceTracker>(progress: &RunProgress<T>) -> u32 {
    match progress {
        RunProgress::FunctionCall(_) => PROGRESS_FUNCTION_CALL,
        RunProgress::OsCall(_) => PROGRESS_OS_CALL,
        RunProgress::ResolveFutures(_) => PROGRESS_RESOLVE_FUTURES,
        RunProgress::NameLookup(_) => PROGRESS_NAME_LOOKUP,
        RunProgress::Complete(_) => PROGRESS_COMPLETE,
    }
}

const fn repl_progress_kind<T: ResourceTracker>(progress: &ReplProgress<T>) -> u32 {
    match progress {
        ReplProgress::FunctionCall(_) => PROGRESS_FUNCTION_CALL,
        ReplProgress::OsCall(_) => PROGRESS_OS_CALL,
        ReplProgress::ResolveFutures(_) => PROGRESS_RESOLVE_FUTURES,
        ReplProgress::NameLookup(_) => PROGRESS_NAME_LOOKUP,
        ReplProgress::Complete { .. } => PROGRESS_COMPLETE,
    }
}

unsafe fn init_progress_snapshot(out: *mut MgProgressSnapshot, kind: u32) {
    // SAFETY: caller provides a non-null output pointer.
    unsafe {
        *out = MgProgressSnapshot {
            kind,
            call_id: 0,
            method_call: 0,
            _pad: [0; 7],
            name: MgBytes::empty(),
            args: raw_value(KIND_INVALID),
            kwargs: raw_value(KIND_INVALID),
            value: raw_value(KIND_INVALID),
        };
    }
}

unsafe fn write_call_snapshot(
    out: *mut MgProgressSnapshot,
    name: &str,
    args: &[MontyObject],
    kwargs: &[(MontyObject, MontyObject)],
    call_id: u32,
    method_call: bool,
) -> Result<(), MgError> {
    // SAFETY: caller provides a non-null output pointer.
    unsafe {
        write_string(ptr::addr_of_mut!((*out).name), name)?;
        write_raw_value(
            ptr::addr_of_mut!((*out).args),
            MontyObject::List(args.to_vec()),
        )?;
        write_raw_value(
            ptr::addr_of_mut!((*out).kwargs),
            MontyObject::Dict(kwargs.to_vec().into()),
        )?;
        (*out).call_id = call_id;
        (*out).method_call = u8::from(method_call);
    }
    Ok(())
}

unsafe fn write_run_snapshot_ref<T: ResourceTracker>(
    out: *mut MgProgressSnapshot,
    progress: &RunProgress<T>,
) -> Result<(), MgError> {
    // SAFETY: caller provides a non-null output pointer.
    unsafe {
        init_progress_snapshot(out, run_progress_kind(progress));
        match progress {
            RunProgress::Complete(value) => {
                write_raw_value(ptr::addr_of_mut!((*out).value), value.clone())
            }
            RunProgress::FunctionCall(call) => write_call_snapshot(
                out,
                &call.function_name,
                &call.args,
                &call.kwargs,
                call.call_id,
                call.method_call,
            ),
            RunProgress::OsCall(call) => write_call_snapshot(
                out,
                &call.function.to_string(),
                &call.args,
                &call.kwargs,
                call.call_id,
                false,
            ),
            RunProgress::NameLookup(lookup) => {
                write_string(ptr::addr_of_mut!((*out).name), &lookup.name)
            }
            RunProgress::ResolveFutures(_) => Ok(()),
        }
    }
}

unsafe fn write_repl_snapshot_ref<T: ResourceTracker>(
    out: *mut MgProgressSnapshot,
    progress: &ReplProgress<T>,
) -> Result<(), MgError> {
    // SAFETY: caller provides a non-null output pointer.
    unsafe {
        init_progress_snapshot(out, repl_progress_kind(progress));
        match progress {
            ReplProgress::Complete { value, .. } => {
                write_raw_value(ptr::addr_of_mut!((*out).value), value.clone())
            }
            ReplProgress::FunctionCall(call) => write_call_snapshot(
                out,
                &call.function_name,
                &call.args,
                &call.kwargs,
                call.call_id,
                call.method_call,
            ),
            ReplProgress::OsCall(call) => write_call_snapshot(
                out,
                &call.function.to_string(),
                &call.args,
                &call.kwargs,
                call.call_id,
                false,
            ),
            ReplProgress::NameLookup(lookup) => {
                write_string(ptr::addr_of_mut!((*out).name), &lookup.name)
            }
            ReplProgress::ResolveFutures(_) => Ok(()),
        }
    }
}

pub unsafe fn write_progress_snapshot_ref(
    out: *mut MgProgressSnapshot,
    progress: &MgProgress,
) -> Result<(), MgError> {
    // SAFETY: forwarded; caller provides a non-null output pointer.
    unsafe {
        match progress {
            MgProgress::NoLimit(p) => write_run_snapshot_ref(out, p),
            MgProgress::Limited(p) => write_run_snapshot_ref(out, p),
            MgProgress::ReplNoLimit(p) => write_repl_snapshot_ref(out, p),
            MgProgress::ReplLimited(p) => write_repl_snapshot_ref(out, p),
        }
    }
}

fn snapshot_output_error(out: *mut MgProgressSnapshotOutput) -> *mut *mut MgError {
    if out.is_null() {
        return ptr::null_mut();
    }
    // SAFETY: out is non-null and points to a caller-provided output struct.
    unsafe { ptr::addr_of_mut!((*out).error) }
}

unsafe fn init_snapshot_output(out: *mut MgProgressSnapshotOutput) {
    // SAFETY: caller provides a non-null output pointer.
    unsafe {
        (*out).progress = ptr::null_mut();
        (*out).repl = ptr::null_mut();
        (*out).error = ptr::null_mut();
        (*out).print = MgBytes::empty();
        (*out).print_flags = PRINT_PLAIN;
        init_progress_snapshot(ptr::addr_of_mut!((*out).snapshot), 0);
    }
}

/// Fills the output struct from a step outcome (or start outcome) plus the
/// hop's print buffer. Returns `true` when the outcome was a failure, so the
/// entry point can return `STATUS_ERR` while still handing back the recovered
/// REPL session.
pub unsafe fn write_step_outcome(
    out: *mut MgProgressSnapshotOutput,
    outcome: Result<StepOutcome, StepFailure>,
    print: &mut PrintBuf,
) -> Result<bool, MgError> {
    if out.is_null() {
        return Err(ffi_error(
            "TypeError",
            "progress snapshot output pointer is null",
        ));
    }
    // SAFETY: out is checked for null above.
    unsafe {
        init_snapshot_output(out);
        print.finish(
            ptr::addr_of_mut!((*out).print),
            ptr::addr_of_mut!((*out).print_flags),
        )?;
        match outcome {
            Ok(StepOutcome::Paused(progress)) => {
                write_progress_snapshot_ref(ptr::addr_of_mut!((*out).snapshot), &progress)?;
                (*out).progress = Box::into_raw(progress);
                Ok(false)
            }
            Ok(StepOutcome::Done { value, repl }) => {
                init_progress_snapshot(ptr::addr_of_mut!((*out).snapshot), PROGRESS_COMPLETE);
                write_raw_value(ptr::addr_of_mut!((*out).snapshot.value), value)?;
                if let Some(inner) = repl {
                    (*out).repl = Box::into_raw(Box::new(MgRepl(Some(*inner))));
                }
                Ok(false)
            }
            Err(failure) => {
                if let Some(inner) = failure.repl {
                    (*out).repl = Box::into_raw(Box::new(MgRepl(Some(*inner))));
                }
                (*out).error = Box::into_raw(Box::new(failure.error));
                Ok(true)
            }
        }
    }
}

/// Shared body for every resume entry point.
///
/// The progress handle is consumed only once the payload decoded: a payload
/// error returns `STATUS_ERR_RETAINED` and the caller keeps the live handle,
/// so a bad resume value cannot brick (or leak) a paused execution.
unsafe fn resume_entry(
    progress: *mut MgProgress,
    out: *mut MgProgressSnapshotOutput,
    action: impl FnOnce() -> Result<Action, MgError>,
) -> i32 {
    let mut failed = false;
    let mut consumed = false;
    let status = guard(snapshot_output_error(out), || {
        if progress.is_null() || out.is_null() {
            return Err(ffi_error(
                "TypeError",
                "progress handle or output pointer is null",
            ));
        }
        let action = action()?;
        consumed = true;
        // SAFETY: progress is consumed exactly once by this call.
        let progress = *unsafe { Box::from_raw(progress) };
        let mut print = PrintBuf::new();
        let outcome = step_progress(progress, action, &mut print);
        // SAFETY: out is checked for null above.
        failed = unsafe { write_step_outcome(out, outcome, &mut print) }?;
        Ok(())
    });
    if status != STATUS_OK {
        return if consumed {
            status
        } else {
            STATUS_ERR_RETAINED
        };
    }
    if failed { STATUS_ERR } else { STATUS_OK }
}

// ---------------------------------------------------------------------------
// Entry points
// ---------------------------------------------------------------------------

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_progress_free(progress: *mut MgProgress) {
    if !progress.is_null() {
        // SAFETY: progress handles are allocated with Box::into_raw in this crate.
        unsafe { drop(Box::from_raw(progress)) };
    }
}

/// Writes the snapshot for a live progress handle without consuming it (used
/// after `mg_progress_load`).
#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_progress_snapshot(
    progress: *const MgProgress,
    out: *mut MgProgressSnapshot,
    out_error: *mut *mut MgError,
) -> i32 {
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
        MgProgress::ReplNoLimit(ReplProgress::ResolveFutures(state)) => {
            Some(f(state.pending_call_ids()))
        }
        MgProgress::ReplLimited(ReplProgress::ResolveFutures(state)) => {
            Some(f(state.pending_call_ids()))
        }
        _ => None,
    }
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
pub unsafe extern "C" fn mg_progress_resume_return_raw(
    progress: *mut MgProgress,
    value: *mut MgRawValue,
    out: *mut MgProgressSnapshotOutput,
) -> i32 {
    // SAFETY: forwarded to resume_entry.
    unsafe {
        resume_entry(progress, out, || {
            if value.is_null() {
                return Err(ffi_error("TypeError", "raw value pointer is null"));
            }
            // SAFETY: value is checked for null above and only read during this call.
            match read_raw_value(&mut *value) {
                Ok(value) => Ok(Action::Return(ExtFunctionResult::Return(value))),
                Err(error) => {
                    // Reclaim any owned handle the failed read left behind so
                    // the retained-error path frees the payload exactly once.
                    free_owned_raw_handle(&mut *value);
                    Err(error)
                }
            }
        })
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_progress_resume_exception(
    progress: *mut MgProgress,
    exc_type_ptr: *const u8,
    exc_type_len: usize,
    message_ptr: *const u8,
    message_len: usize,
    out: *mut MgProgressSnapshotOutput,
) -> i32 {
    // SAFETY: forwarded to resume_entry.
    unsafe {
        resume_entry(progress, out, || {
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
            Ok(Action::Return(ExtFunctionResult::Error(exception)))
        })
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_progress_resume_pending(
    progress: *mut MgProgress,
    out: *mut MgProgressSnapshotOutput,
) -> i32 {
    // SAFETY: forwarded to resume_entry.
    unsafe { resume_entry(progress, out, || Ok(Action::Pending)) }
}

/// Resumes a paused OS call using Monty's default unhandled behavior
/// (`PermissionError` for filesystem functions, `RuntimeError` otherwise).
#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_progress_resume_not_handled(
    progress: *mut MgProgress,
    out: *mut MgProgressSnapshotOutput,
) -> i32 {
    // SAFETY: forwarded to resume_entry.
    unsafe { resume_entry(progress, out, || Ok(Action::NotHandled)) }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_progress_resume_name_value_raw(
    progress: *mut MgProgress,
    value: *mut MgRawValue,
    out: *mut MgProgressSnapshotOutput,
) -> i32 {
    // SAFETY: forwarded to resume_entry.
    unsafe {
        resume_entry(progress, out, || {
            if value.is_null() {
                return Err(ffi_error("TypeError", "raw value pointer is null"));
            }
            // SAFETY: value is checked for null above and only read during this call.
            match read_raw_value(&mut *value) {
                Ok(value) => Ok(Action::Name(NameLookupResult::Value(value))),
                Err(error) => {
                    free_owned_raw_handle(&mut *value);
                    Err(error)
                }
            }
        })
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_progress_resume_name_undefined(
    progress: *mut MgProgress,
    out: *mut MgProgressSnapshotOutput,
) -> i32 {
    // SAFETY: forwarded to resume_entry.
    unsafe {
        resume_entry(progress, out, || {
            Ok(Action::Name(NameLookupResult::Undefined))
        })
    }
}

pub fn read_future_results(
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
    let results = unsafe { std::slice::from_raw_parts_mut(ptr, len) };
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
            FUTURE_RESULT_NOT_FOUND => match crate::as_str(results[i].message) {
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

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_progress_resume_futures(
    progress: *mut MgProgress,
    results: *mut MgFutureResult,
    results_len: usize,
    out: *mut MgProgressSnapshotOutput,
) -> i32 {
    // SAFETY: forwarded to resume_entry.
    unsafe {
        resume_entry(progress, out, || {
            Ok(Action::Futures(read_future_results(results, results_len)?))
        })
    }
}

/// Serializes the pending call's positional args and kwargs in Monty's
/// natural JSON form. Valid for function-call and OS-call states.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_progress_call_json(
    progress: *const MgProgress,
    out_args: *mut MgBytes,
    out_kwargs: *mut MgBytes,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if progress.is_null() {
            return Err(ffi_error("TypeError", "progress handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        let (args, kwargs): (&[MontyObject], &[(MontyObject, MontyObject)]) = match unsafe {
            &*progress
        } {
            MgProgress::NoLimit(RunProgress::FunctionCall(call)) => (&call.args, &call.kwargs),
            MgProgress::Limited(RunProgress::FunctionCall(call)) => (&call.args, &call.kwargs),
            MgProgress::NoLimit(RunProgress::OsCall(call)) => (&call.args, &call.kwargs),
            MgProgress::Limited(RunProgress::OsCall(call)) => (&call.args, &call.kwargs),
            MgProgress::ReplNoLimit(ReplProgress::FunctionCall(call)) => (&call.args, &call.kwargs),
            MgProgress::ReplLimited(ReplProgress::FunctionCall(call)) => (&call.args, &call.kwargs),
            MgProgress::ReplNoLimit(ReplProgress::OsCall(call)) => (&call.args, &call.kwargs),
            MgProgress::ReplLimited(ReplProgress::OsCall(call)) => (&call.args, &call.kwargs),
            _ => {
                return Err(ffi_error(
                    "RuntimeError",
                    "progress state has no pending call",
                ));
            }
        };
        let args_json = serde_json::to_vec(&JsonMontyArray(args))
            .map_err(|err| ffi_error("SerializationError", err.to_string()))?;
        let kwargs_json = serde_json::to_vec(&JsonMontyPairs(kwargs))
            .map_err(|err| ffi_error("SerializationError", err.to_string()))?;
        write_owned_bytes(out_args, args_json)?;
        write_owned_bytes(out_kwargs, kwargs_json)
    })
}

/// Attaches a cancellation token to a loaded progress handle. Returns 1 when
/// attached; 0 when the paused state does not expose its tracker (only
/// tracked function-call states do upstream).
#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_progress_set_cancel_token(
    progress: *mut MgProgress,
    token: *mut MgCancelToken,
) -> u8 {
    if progress.is_null() {
        return 0;
    }
    let cancel = if token.is_null() {
        None
    } else {
        // SAFETY: token handles are allocated with Box::into_raw in this crate.
        Some(unsafe { (*token).0.clone() })
    };
    // SAFETY: handle validity is owned by the Go side contract.
    match unsafe { &mut *progress } {
        MgProgress::Limited(RunProgress::FunctionCall(call)) => {
            call.tracker_mut().set_cancel(cancel);
            1
        }
        _ => 0,
    }
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
        write_owned_bytes(out, bytes)
    })
}

/// Reports whether a loaded progress handle is a REPL execution (its
/// completion will hand back a session via `out.repl`).
#[unsafe(no_mangle)]
pub const unsafe extern "C" fn mg_progress_is_repl(progress: *const MgProgress) -> u8 {
    if progress.is_null() {
        return 0;
    }
    // SAFETY: handle validity is owned by the Go side contract.
    match unsafe { &*progress } {
        MgProgress::NoLimit(_) | MgProgress::Limited(_) => 0,
        MgProgress::ReplNoLimit(_) | MgProgress::ReplLimited(_) => 1,
    }
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
