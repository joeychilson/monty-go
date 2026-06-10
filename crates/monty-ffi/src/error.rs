//! Error materialization.
//!
//! `mg_error_details` packs everything the Go side needs into one flat buffer
//! so a failed run costs two FFI calls (details + free) instead of five:
//!
//! ```text
//! u32 type_len    + bytes
//! u32 message_len + bytes
//! u32 display_len + bytes
//! u32 frame_count
//! per frame:
//!   u32 line, u32 column, u32 end_line, u32 end_column
//!   u8  flags   (bit0 = has function name, bit1 = has source line,
//!                bit2 = hide caret,        bit3 = hide frame name)
//!   u32 filename_len + bytes
//!   [u32 len + bytes]  function name (when bit0)
//!   [u32 len + bytes]  source line   (when bit1)
//! ```
//!
//! Frames are ordered outermost-first, matching upstream traceback order.
//! Synthetic (non-Python) errors have zero frames.
//! All little-endian.

use crate::{
    MgBytes, MgError, ffi_error, guard, push_flat_bytes, push_flat_u32, write_owned_bytes,
};

const FRAME_HAS_FUNCTION: u8 = 1;
const FRAME_HAS_SOURCE: u8 = 1 << 1;
const FRAME_HIDE_CARET: u8 = 1 << 2;
const FRAME_HIDE_FRAME_NAME: u8 = 1 << 3;

fn encode_details(error: &MgError) -> Result<Vec<u8>, MgError> {
    let frames = error.exc.as_ref().map_or(&[][..], |exc| exc.traceback());
    let mut out = Vec::with_capacity(
        16 + error.exc_type.len() + error.message.len() + error.display.len() + frames.len() * 64,
    );
    push_flat_bytes(&mut out, error.exc_type.as_bytes())?;
    push_flat_bytes(&mut out, error.message.as_bytes())?;
    push_flat_bytes(&mut out, error.display.as_bytes())?;
    push_flat_u32(&mut out, frames.len())?;
    for frame in frames {
        push_flat_u32(&mut out, frame.start.line as usize)?;
        push_flat_u32(&mut out, frame.start.column as usize)?;
        push_flat_u32(&mut out, frame.end.line as usize)?;
        push_flat_u32(&mut out, frame.end.column as usize)?;
        let mut flags = 0u8;
        if frame.frame_name.is_some() {
            flags |= FRAME_HAS_FUNCTION;
        }
        if frame.preview_line.is_some() {
            flags |= FRAME_HAS_SOURCE;
        }
        if frame.hide_caret {
            flags |= FRAME_HIDE_CARET;
        }
        if frame.hide_frame_name {
            flags |= FRAME_HIDE_FRAME_NAME;
        }
        out.push(flags);
        push_flat_bytes(&mut out, frame.filename.as_bytes())?;
        if let Some(name) = &frame.frame_name {
            push_flat_bytes(&mut out, name.as_bytes())?;
        }
        if let Some(line) = &frame.preview_line {
            push_flat_bytes(&mut out, line.as_bytes())?;
        }
    }
    Ok(out)
}

/// Writes the flat details buffer (type, message, display, traceback) for an
/// error handle. The buffer is released with `mg_bytes_free`.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_error_details(
    error: *const MgError,
    out: *mut MgBytes,
    out_error: *mut *mut MgError,
) -> i32 {
    guard(out_error, || {
        if error.is_null() {
            return Err(ffi_error("TypeError", "error handle is null"));
        }
        // SAFETY: handle validity is owned by the Go side contract.
        let details = encode_details(unsafe { &*error })?;
        write_owned_bytes(out, details)
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_error_free(error: *mut MgError) {
    if !error.is_null() {
        // SAFETY: error handles are allocated with Box::into_raw in this crate.
        unsafe { drop(Box::from_raw(error)) };
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::from_monty_error;
    use monty::{MontyRun, NoLimitTracker, PrintWriter};

    fn read_u32(buf: &[u8], pos: &mut usize) -> u32 {
        let value = u32::from_le_bytes(buf[*pos..*pos + 4].try_into().unwrap());
        *pos += 4;
        value
    }

    fn read_str(buf: &[u8], pos: &mut usize) -> String {
        let len = read_u32(buf, pos) as usize;
        let value = String::from_utf8(buf[*pos..*pos + len].to_vec()).unwrap();
        *pos += len;
        value
    }

    #[test]
    fn details_round_trip_with_traceback() {
        let runner = MontyRun::new(
            "def boom():\n    raise ValueError('nope')\nboom()".to_owned(),
            "test.py",
            Vec::new(),
        )
        .unwrap();
        let exc = runner
            .run(Vec::new(), NoLimitTracker, PrintWriter::Disabled)
            .unwrap_err();
        let error = from_monty_error(exc);
        let buf = encode_details(&error).unwrap();

        let mut pos = 0;
        assert_eq!(read_str(&buf, &mut pos), "ValueError");
        assert_eq!(read_str(&buf, &mut pos), "nope");
        let display = read_str(&buf, &mut pos);
        assert!(display.contains("ValueError: nope"), "display: {display}");
        let frame_count = read_u32(&buf, &mut pos);
        assert!(frame_count >= 2, "expected module + function frames");
        for _ in 0..frame_count {
            let line = read_u32(&buf, &mut pos);
            assert!(line >= 1);
            let _column = read_u32(&buf, &mut pos);
            let _end_line = read_u32(&buf, &mut pos);
            let _end_column = read_u32(&buf, &mut pos);
            let flags = buf[pos];
            pos += 1;
            let filename = read_str(&buf, &mut pos);
            assert_eq!(filename, "test.py");
            if flags & FRAME_HAS_FUNCTION != 0 {
                let _ = read_str(&buf, &mut pos);
            }
            if flags & FRAME_HAS_SOURCE != 0 {
                let _ = read_str(&buf, &mut pos);
            }
        }
        assert_eq!(pos, buf.len());
    }

    #[test]
    fn details_synthetic_error_has_no_frames() {
        let error = ffi_error("TypeError", "boom");
        let buf = encode_details(&error).unwrap();
        let mut pos = 0;
        assert_eq!(read_str(&buf, &mut pos), "TypeError");
        assert_eq!(read_str(&buf, &mut pos), "boom");
        assert_eq!(read_str(&buf, &mut pos), "TypeError: boom");
        assert_eq!(read_u32(&buf, &mut pos), 0);
        assert_eq!(pos, buf.len());
    }
}
