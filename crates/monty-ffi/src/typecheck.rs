//! Static type checking with structured diagnostics.
//!
//! `mg_type_check` yields an `MgDiagnostics` handle when the code has type
//! errors (and null when clean). The handle renders in any of the upstream
//! formats via `mg_diagnostics_render`; the Go side obtains structured
//! per-diagnostic data by rendering `"json"` once and parsing it.

use std::sync::Mutex;

use monty_type_checking::{SourceFile, type_check};

use crate::{
    MgBytes, MgDiagnostics, MgError, MgTypeCheckArgs, as_str, ffi_error, guard, write_owned_string,
};

/// Type-checks code. On success with diagnostics, `*out_diags` receives an
/// owned handle (release with `mg_diagnostics_free`); on a clean pass it is
/// set to null. Infrastructure failures are reported through `out_error`.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_type_check(
    args: *const MgTypeCheckArgs,
    out_diags: *mut *mut MgDiagnostics,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if args.is_null() {
            return Err(ffi_error("TypeError", "type check args pointer is null"));
        }
        if out_diags.is_null() {
            return Err(ffi_error("TypeError", "diagnostics output pointer is null"));
        }
        // SAFETY: args is checked for null above and only read during this call.
        let args = unsafe { &*args };
        let code = as_str(args.code)?;
        let script_name = as_str(args.script_name)?;
        let stubs = as_str(args.stubs)?;
        let stubs_name = as_str(args.stubs_name)?;
        let stubs_file = (!stubs.is_empty()).then(|| SourceFile::new(stubs, stubs_name));
        let result = type_check(&SourceFile::new(code, script_name), stubs_file.as_ref())
            .map_err(|message| ffi_error("RuntimeError", message))?;
        // SAFETY: out_diags is checked for null above.
        unsafe {
            *out_diags = result.map_or(std::ptr::null_mut(), |diagnostics| {
                Box::into_raw(Box::new(MgDiagnostics {
                    inner: Mutex::new(Some(diagnostics)),
                }))
            });
        }
        Ok(())
    })
}

/// Renders diagnostics in one of the upstream formats ("full", "concise",
/// "azure", "json", "jsonlines", "rdjson", "pylint", "gitlab", "github").
#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_diagnostics_render(
    diags: *const MgDiagnostics,
    format_ptr: *const u8,
    format_len: usize,
    color: u8,
    out: *mut MgBytes,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if diags.is_null() {
            return Err(ffi_error("TypeError", "diagnostics handle is null"));
        }
        let format = crate::str_arg(format_ptr, format_len)?;
        // SAFETY: handle validity is owned by the Go side contract.
        let slot = unsafe { &(*diags).inner };
        let rendered = {
            let mut guard = slot
                .lock()
                .map_err(|_| ffi_error("RuntimeError", "diagnostics handle is poisoned"))?;
            // The upstream builder methods consume self, so move the value
            // out, configure, render, and put the configured value back.
            let diagnostics = guard
                .take()
                .ok_or_else(|| ffi_error("RuntimeError", "diagnostics handle already consumed"))?;
            let diagnostics = diagnostics
                .format_from_str(format)
                .map_err(|message| ffi_error("ValueError", message))?
                .color(color != 0);
            let rendered = diagnostics.to_string();
            *guard = Some(diagnostics);
            rendered
        };
        write_owned_string(out, rendered)
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_diagnostics_free(diags: *mut MgDiagnostics) {
    if !diags.is_null() {
        // SAFETY: diagnostics handles are allocated with Box::into_raw in this crate.
        unsafe { drop(Box::from_raw(diags)) };
    }
}
