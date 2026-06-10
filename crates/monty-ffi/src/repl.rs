//! Stateful REPL sessions.
//!
//! `MgRepl` is a slot: `mg_repl_feed_start` consumes the session (upstream
//! `feed_start` takes `self` so runtime state can move into the progress
//! snapshot), leaving the slot empty; the updated session returns through
//! `MgProgressSnapshotOutput.repl` at completion or with a preserved-session
//! error. `mg_repl_feed_run_raw` drives the same machinery to completion in
//! one hop — dispatching host functions, mounts, and OS calls — and always
//! puts the session back into the slot before returning.

use std::ptr;

use monty::{
    MontyObject, MontyRepl, NoLimitTracker, ReplContinuationMode, ReplProgress, ResourceTracker,
    detect_repl_continuation_mode,
};

use crate::program::{HostDispatch, write_fast_output};
use crate::progress::{StepFailure, StepOutcome, write_step_outcome};
use crate::{
    MgBytes, MgError, MgProgress, MgProgressSnapshotOutput, MgRepl, MgReplCallArgs, MgReplFeedArgs,
    MgReplNewArgs, MgRunFastOutput, MgValue, PrintBuf, ReplInner, STATUS_ERR, STATUS_OK, as_bytes,
    as_str, ffi_error, from_monty_error, guard, limits_from_raw, read_raw_values, read_string_list,
    str_arg, string_list_value, write_owned_bytes,
};

// ---------------------------------------------------------------------------
// Slot helpers
// ---------------------------------------------------------------------------

fn repl_slot<'a>(repl: *mut MgRepl) -> Result<&'a mut MgRepl, MgError> {
    if repl.is_null() {
        return Err(ffi_error("TypeError", "REPL handle is null"));
    }
    // SAFETY: handle validity is owned by the Go side contract.
    Ok(unsafe { &mut *repl })
}

fn consumed_error() -> MgError {
    ffi_error(
        "RuntimeError",
        "REPL session was consumed by feed_start and has not completed",
    )
}

/// Applies per-snippet execution controls (time budget, cancellation) to a
/// session about to run a snippet.
fn apply_feed_controls(inner: &mut ReplInner, args: &MgReplFeedArgs) -> Result<(), MgError> {
    let cancel = if args.cancel_token.is_null() {
        None
    } else {
        // SAFETY: cancel token handles are allocated with Box::into_raw in this crate.
        Some(unsafe { (*args.cancel_token).0.clone() })
    };
    match inner {
        ReplInner::Limited(repl) => {
            let tracker = repl.tracker_mut();
            tracker.set_cancel(cancel);
            // Set or clear the budget every feed so one snippet's deadline
            // never leaks into the next.
            tracker.set_snippet_deadline(
                (args.has_max_duration != 0)
                    .then(|| std::time::Duration::from_nanos(args.max_duration_nanos)),
            );
            Ok(())
        }
        ReplInner::NoLimit(_) => {
            if args.has_max_duration != 0 || cancel.is_some() {
                return Err(ffi_error(
                    "ValueError",
                    "per-snippet limits require a REPL created with limits",
                ));
            }
            Ok(())
        }
    }
}

fn read_feed_inputs(args: &MgReplFeedArgs) -> Result<Vec<(String, MontyObject)>, MgError> {
    let names = read_string_list(args.input_names, args.input_count)?;
    let values = read_raw_values(args.input_values, args.input_count)?;
    Ok(names.into_iter().zip(values).collect())
}

// ---------------------------------------------------------------------------
// Feed-run dispatch loop
// ---------------------------------------------------------------------------

/// Drives a snippet to completion, resolving every pause through the host
/// dispatch. Returns the final value and the recovered session in all cases —
/// including failures, so the slot can always be restored.
fn feed_run_loop<T: ResourceTracker>(
    repl: MontyRepl<T>,
    code: &str,
    inputs: Vec<(String, MontyObject)>,
    dispatch: &HostDispatch<'_>,
    print: &mut PrintBuf,
) -> (Result<MontyObject, MgError>, Option<MontyRepl<T>>) {
    let mut progress = match repl.feed_start(code, inputs, print.writer()) {
        Ok(progress) => progress,
        Err(start_error) => {
            return (
                Err(from_monty_error(start_error.error)),
                Some(start_error.repl),
            );
        }
    };
    loop {
        let result = match progress {
            ReplProgress::Complete { repl, value } => return (Ok(value), Some(repl)),
            ReplProgress::NameLookup(lookup) => {
                let result = dispatch.resolve_name(&lookup.name);
                lookup.resume(result, print.writer())
            }
            ReplProgress::FunctionCall(call) => {
                let result =
                    match dispatch.function_result(&call.function_name, &call.args, &call.kwargs) {
                        Ok(result) => result,
                        Err(error) => return (Err(error), Some(call.into_repl())),
                    };
                call.resume(result, print.writer())
            }
            ReplProgress::OsCall(call) => {
                let result = match dispatch.os_result(call.function, &call.args, &call.kwargs) {
                    Ok(result) => result,
                    Err(error) => return (Err(error), Some(call.into_repl())),
                };
                call.resume(result, print.writer())
            }
            ReplProgress::ResolveFutures(state) => {
                return (
                    Err(ffi_error(
                        "RuntimeError",
                        "REPL feed_run does not support pending futures; use feed_start",
                    )),
                    Some(state.into_repl()),
                );
            }
        };
        progress = match result {
            Ok(next) => next,
            Err(start_error) => {
                return (
                    Err(from_monty_error(start_error.error)),
                    Some(start_error.repl),
                );
            }
        };
    }
}

// ---------------------------------------------------------------------------
// Entry points
// ---------------------------------------------------------------------------

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
        let spec = limits_from_raw(args.limits);
        let inner = if spec.tracked() {
            ReplInner::Limited(MontyRepl::new(script_name, spec.tracker()))
        } else {
            ReplInner::NoLimit(MontyRepl::new(script_name, NoLimitTracker))
        };
        // SAFETY: out_repl is checked for null above.
        unsafe { *out_repl = Box::into_raw(Box::new(MgRepl(Some(inner)))) };
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
        let inner = unsafe { (*repl).0.as_ref() }.ok_or_else(consumed_error)?;
        let bytes = postcard::to_allocvec(inner)
            .map_err(|err| ffi_error("SerializationError", err.to_string()))?;
        write_owned_bytes(out, bytes)
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
        let inner: ReplInner = postcard::from_bytes(bytes)
            .map_err(|err| ffi_error("SerializationError", err.to_string()))?;
        // SAFETY: out_repl is checked for null above.
        unsafe { *out_repl = Box::into_raw(Box::new(MgRepl(Some(inner)))) };
        Ok(())
    })
}

/// Executes one snippet to completion with full host dispatch. The session
/// handle remains valid afterwards regardless of outcome.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_repl_feed_run_raw(
    repl: *mut MgRepl,
    args: *const MgReplFeedArgs,
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
            return Err(ffi_error("TypeError", "REPL feed args pointer is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "fast value output pointer is null"));
        }
        // SAFETY: args is checked for null above and only read during this call.
        let args = unsafe { &*args };
        let code = as_str(args.code)?;
        let inputs = read_feed_inputs(args)?;
        let slot = repl_slot(repl)?;
        let mut inner = slot.0.take().ok_or_else(consumed_error)?;
        if let Err(error) = apply_feed_controls(&mut inner, args) {
            slot.0 = Some(inner);
            return Err(error);
        }

        let (mut dispatch, names) = HostDispatch::from_feed_args(args)?;
        dispatch.host_names = &names;
        let mut print = PrintBuf::for_callback(args.print, args.print_data);

        let (result, recovered) = match inner {
            ReplInner::NoLimit(session) => {
                let (result, recovered) =
                    feed_run_loop(session, code, inputs, &dispatch, &mut print);
                (result, recovered.map(ReplInner::NoLimit))
            }
            ReplInner::Limited(session) => {
                let (result, recovered) =
                    feed_run_loop(session, code, inputs, &dispatch, &mut print);
                (result, recovered.map(ReplInner::Limited))
            }
        };
        slot.0 = recovered;
        let value = result?;
        // SAFETY: out is checked for null above.
        unsafe { write_fast_output(out, value, &mut print) }
    })
}

/// Starts a suspendable snippet execution, consuming the session: it moves
/// into the returned progress and comes back via `out.repl` at completion or
/// with a preserved-session error.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_repl_feed_start(
    repl: *mut MgRepl,
    args: *const MgReplFeedArgs,
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
        if args.is_null() {
            return Err(ffi_error("TypeError", "REPL feed args pointer is null"));
        }
        if out.is_null() {
            return Err(ffi_error(
                "TypeError",
                "progress snapshot output pointer is null",
            ));
        }
        // SAFETY: args is checked for null above and only read during this call.
        let args = unsafe { &*args };
        if args.callback.is_some() || args.mount_count != 0 || args.host_name_count != 0 {
            return Err(ffi_error(
                "ValueError",
                "feed_start does not dispatch host calls; drive them via resume",
            ));
        }
        let code = as_str(args.code)?;
        let inputs = read_feed_inputs(args)?;
        let slot = repl_slot(repl)?;
        let mut inner = slot.0.take().ok_or_else(consumed_error)?;
        if let Err(error) = apply_feed_controls(&mut inner, args) {
            slot.0 = Some(inner);
            return Err(error);
        }

        let mut print = PrintBuf::new();
        let outcome: Result<StepOutcome, StepFailure> = match inner {
            ReplInner::NoLimit(session) => match session.feed_start(code, inputs, print.writer()) {
                Ok(ReplProgress::Complete { repl, value }) => Ok(StepOutcome::Done {
                    value,
                    repl: Some(Box::new(ReplInner::NoLimit(repl))),
                }),
                Ok(progress) => Ok(StepOutcome::Paused(Box::new(MgProgress::ReplNoLimit(
                    progress,
                )))),
                Err(start_error) => Err(StepFailure {
                    error: from_monty_error(start_error.error),
                    repl: Some(Box::new(ReplInner::NoLimit(start_error.repl))),
                }),
            },
            ReplInner::Limited(session) => match session.feed_start(code, inputs, print.writer()) {
                Ok(ReplProgress::Complete { repl, value }) => Ok(StepOutcome::Done {
                    value,
                    repl: Some(Box::new(ReplInner::Limited(repl))),
                }),
                Ok(progress) => Ok(StepOutcome::Paused(Box::new(MgProgress::ReplLimited(
                    progress,
                )))),
                Err(start_error) => Err(StepFailure {
                    error: from_monty_error(start_error.error),
                    repl: Some(Box::new(ReplInner::Limited(start_error.repl))),
                }),
            },
        };
        // SAFETY: out is checked for null above.
        failed = unsafe { write_step_outcome(out, outcome, &mut print) }?;
        Ok(())
    });
    if status != STATUS_OK {
        return status;
    }
    if failed { STATUS_ERR } else { STATUS_OK }
}

/// Calls a function defined in the session by name.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_repl_call_raw(
    repl: *mut MgRepl,
    args: *const MgReplCallArgs,
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
            return Err(ffi_error("TypeError", "REPL call args pointer is null"));
        }
        if out.is_null() {
            return Err(ffi_error("TypeError", "fast value output pointer is null"));
        }
        // SAFETY: args is checked for null above and only read during this call.
        let args = unsafe { &*args };
        let name = as_str(args.name)?;
        let call_args = read_raw_values(args.args, args.arg_count)?;
        let slot = repl_slot(repl)?;
        let inner = slot.0.as_mut().ok_or_else(consumed_error)?;
        let mut print = PrintBuf::new();
        let value = match inner {
            ReplInner::NoLimit(session) => session.call_function(name, call_args, print.writer()),
            ReplInner::Limited(session) => session.call_function(name, call_args, print.writer()),
        }
        .map_err(from_monty_error)?;
        // SAFETY: out is checked for null above.
        unsafe { write_fast_output(out, value, &mut print) }
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
        let inner = unsafe { (*repl).0.as_ref() }.ok_or_else(consumed_error)?;
        let names: Vec<String> = match inner {
            ReplInner::NoLimit(session) => session
                .function_names()
                .into_iter()
                .map(str::to_owned)
                .collect(),
            ReplInner::Limited(session) => session
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
    let Some(inner) = (unsafe { (*repl).0.as_ref() }) else {
        return 0;
    };
    let has_function = match inner {
        ReplInner::NoLimit(session) => session.has_function(name),
        ReplInner::Limited(session) => session.has_function(name),
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
