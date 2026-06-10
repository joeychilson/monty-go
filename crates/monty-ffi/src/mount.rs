//! Filesystem mounts.
//!
//! An `MgMount` owns a `monty::fs::Mount` behind a shared slot so the same
//! mount (including persistent overlay state) can be attached to many runs.
//! `mg_mount_handle_os_call` serves the Go-side dispatch pump; single-hop
//! host runs route OS calls through the same slots inside Rust.

use std::ptr;
use std::sync::{Arc, Mutex};

use monty::fs::{Mount as MontyMount, MountMode as MontyMountMode, MountTable};
use monty::{MontyObject, OsFunction};

use crate::{
    KIND_INVALID, MgError, MgMount, MgMountCallArgs, MgMountNewArgs, MgMountOutput, as_str,
    ffi_error, from_monty_error, guard, raw_value, read_raw_pairs, read_raw_values,
    write_raw_value,
};

pub const MOUNT_MODE_READ_ONLY: u32 = 0;
pub const MOUNT_MODE_READ_WRITE: u32 = 1;
pub const MOUNT_MODE_OVERLAY: u32 = 2;

pub fn mount_mode_from_raw(mode: u32) -> Result<MontyMountMode, MgError> {
    match mode {
        MOUNT_MODE_READ_ONLY => Ok(MontyMountMode::ReadOnly),
        MOUNT_MODE_READ_WRITE => Ok(MontyMountMode::ReadWrite),
        MOUNT_MODE_OVERLAY => {
            MontyMountMode::from_mode_str("overlay").map_err(|err| ffi_error("ValueError", err))
        }
        _ => Err(ffi_error(
            "ValueError",
            "mount mode must be read-only, read-write, or overlay",
        )),
    }
}

/// Collects the shared slots for a borrowed mount-handle array.
pub fn mount_slots(
    mounts: *const *mut MgMount,
    count: usize,
) -> Result<Vec<Arc<Mutex<Option<MontyMount>>>>, MgError> {
    if mounts.is_null() {
        if count == 0 {
            return Ok(Vec::new());
        }
        return Err(ffi_error(
            "TypeError",
            "non-empty mount array has null pointer",
        ));
    }
    // SAFETY: caller promises mounts points to count handles.
    let handles = unsafe { std::slice::from_raw_parts(mounts, count) };
    let mut slots = Vec::with_capacity(handles.len());
    for mount in handles {
        if mount.is_null() {
            return Err(ffi_error("TypeError", "mount handle is null"));
        }
        // SAFETY: mount handle validity is owned by the Go side contract.
        slots.push(unsafe { (*(*mount)).slot.clone() });
    }
    Ok(slots)
}

/// Runs one OS call against the mount table. Returns `Ok(None)` when no mount
/// claims the path, `Ok(Some(result))` when handled, and `Err` for mount-level
/// failures (which carry the Python exception to raise).
pub fn dispatch_mount_os_call(
    slots: &[Arc<Mutex<Option<MontyMount>>>],
    function: OsFunction,
    args: &[MontyObject],
    kwargs: &[(MontyObject, MontyObject)],
) -> Result<Option<MontyObject>, MgError> {
    if slots.is_empty() {
        return Ok(None);
    }
    let mut table =
        MountTable::take_shared_mounts(slots).map_err(|err| ffi_error("RuntimeError", err))?;
    let result = table.handle_os_call(function, args, kwargs);
    table.put_back_shared_mounts(slots);
    match result {
        None => Ok(None),
        Some(Ok(object)) => Ok(Some(object)),
        Some(Err(err)) => Err(from_monty_error(err.into_exception())),
    }
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
        let mode = mount_mode_from_raw(args.mode)?;
        let limit = (args.has_write_bytes_limit != 0).then_some(args.write_bytes_limit);
        let mount = MontyMount::new(virtual_path, host_path, mode, limit)
            .map_err(|err| from_monty_error(err.into_exception()))?;
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

/// Tries the given mounts for one OS call. `out.handled` reports whether any
/// mount claimed the path; the result value (when handled) is written as an
/// owned raw value the caller must consume or free.
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
        // SAFETY: out is checked for null above.
        unsafe {
            (*out).handled = 0;
            (*out).value = raw_value(KIND_INVALID);
            (*out).error = ptr::null_mut();
        }

        let function = as_str(args.function)?
            .parse::<OsFunction>()
            .map_err(|_| ffi_error("ValueError", "unknown OS function"))?;
        let call_args = read_raw_values(args.args, args.arg_count)?;
        let kwargs = read_raw_pairs(args.kwargs, args.kwarg_count)?;
        let slots = mount_slots(args.mounts, args.mount_count)?;

        match dispatch_mount_os_call(&slots, function, &call_args, &kwargs)? {
            None => Ok(()),
            Some(object) => {
                // SAFETY: out is checked for null above.
                unsafe {
                    (*out).handled = 1;
                    write_raw_value(ptr::addr_of_mut!((*out).value), object)?;
                }
                Ok(())
            }
        }
    })
}
