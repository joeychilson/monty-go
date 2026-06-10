//! Print output plumbing.
//!
//! Every execution path collects `print()` output as `(stream, text)` chunks
//! via `PrintWriter::CollectStreams`. At each FFI hop the chunks are encoded
//! into the output struct's `print`/`print_flags` pair:
//!
//! - `PRINT_PLAIN` (0): the buffer is plain stdout text — the only case Monty
//!   produces today, and the zero-overhead path.
//! - `PRINT_TAGGED` (1): the buffer is `[u8 stream][u32 le len][bytes]` chunks
//!   in emit order, preserving stdout/stderr interleaving.
//!
//! Host-dispatch entry points may instead supply a streaming callback
//! (`MgPrintCallback`); output is then flushed to Go on every newline rather
//! than buffered for the hop, and the hop's `print` buffer stays empty.

use std::borrow::Cow;

use monty::{MontyException, PrintStream, PrintWriter, PrintWriterCallback};

use crate::{MgBytes, MgError, MgPrintCallback, PRINT_PLAIN, PRINT_TAGGED, write_owned_bytes};

const STREAM_STDOUT: u8 = 0;
const STREAM_STDERR: u8 = 1;

const fn stream_tag(stream: PrintStream) -> u8 {
    match stream {
        PrintStream::Stdout => STREAM_STDOUT,
        PrintStream::Stderr => STREAM_STDERR,
    }
}

/// Per-hop print collector: either buffered chunks or a streaming callback.
pub enum PrintBuf {
    Collect(Vec<(PrintStream, String)>),
    Stream(StreamingPrint),
}

impl PrintBuf {
    pub(crate) const fn new() -> Self {
        Self::Collect(Vec::new())
    }

    /// A streaming collector when `callback` is provided, else a buffered one.
    pub(crate) fn for_callback(callback: MgPrintCallback, user_data: usize) -> Self {
        callback.map_or_else(Self::new, |callback| {
            Self::Stream(StreamingPrint {
                callback,
                user_data,
                stream: PrintStream::Stdout,
                buf: String::new(),
            })
        })
    }

    pub(crate) fn writer(&mut self) -> PrintWriter<'_> {
        match self {
            Self::Collect(chunks) => PrintWriter::CollectStreams(chunks),
            Self::Stream(stream) => PrintWriter::Callback(stream),
        }
    }

    /// Encodes collected output into `(print, print_flags)` fields and resets
    /// the buffer. Streaming collectors flush any unterminated tail to the
    /// callback instead and leave the buffer empty.
    pub(crate) fn finish(
        &mut self,
        out_print: *mut MgBytes,
        out_flags: *mut u32,
    ) -> Result<(), MgError> {
        match self {
            Self::Collect(chunks) => {
                let (bytes, flags) = encode_chunks(std::mem::take(chunks));
                // SAFETY: callers pass field pointers into a caller-provided
                // output struct; write_owned_bytes checks out_print for null.
                unsafe {
                    if !out_flags.is_null() {
                        *out_flags = flags;
                    }
                }
                write_owned_bytes(out_print, bytes)
            }
            Self::Stream(stream) => {
                stream.flush();
                // SAFETY: as above.
                unsafe {
                    if !out_flags.is_null() {
                        *out_flags = PRINT_PLAIN;
                    }
                    if !out_print.is_null() {
                        *out_print = MgBytes::empty();
                    }
                }
                Ok(())
            }
        }
    }
}

/// Encodes chunks into the wire format, choosing `PRINT_PLAIN` when the output
/// is pure stdout (the common case — zero re-encoding, the single chunk's
/// string is reused as the buffer).
fn encode_chunks(mut chunks: Vec<(PrintStream, String)>) -> (Vec<u8>, u32) {
    match chunks.len() {
        0 => (Vec::new(), PRINT_PLAIN),
        // CollectStreams merges consecutive same-stream fragments, so pure
        // stdout output is always exactly one chunk.
        1 if chunks[0].0 == PrintStream::Stdout => {
            (std::mem::take(&mut chunks[0].1).into_bytes(), PRINT_PLAIN)
        }
        _ => {
            let total: usize = chunks.iter().map(|(_, text)| text.len() + 5).sum();
            let mut bytes = Vec::with_capacity(total);
            for (stream, text) in &chunks {
                bytes.push(stream_tag(*stream));
                bytes.extend_from_slice(&u32::try_from(text.len()).unwrap_or(0).to_le_bytes());
                bytes.extend_from_slice(text.as_bytes());
            }
            (bytes, PRINT_TAGGED)
        }
    }
}

/// `PrintWriterCallback` adaptor that forwards output to a Go callback,
/// buffering fragments and flushing on newlines so a `print(a, b, c)` call
/// reaches Go as one write instead of five.
pub struct StreamingPrint {
    callback: unsafe extern "C" fn(usize, u8, *const u8, usize),
    user_data: usize,
    stream: PrintStream,
    buf: String,
}

impl StreamingPrint {
    fn flush(&mut self) {
        if self.buf.is_empty() {
            return;
        }
        let bytes = self.buf.as_bytes();
        // SAFETY: the callback is provided by Go for the duration of the
        // enclosing FFI call; the buffer outlives the synchronous callback.
        unsafe {
            (self.callback)(
                self.user_data,
                stream_tag(self.stream),
                bytes.as_ptr(),
                bytes.len(),
            );
        }
        self.buf.clear();
    }
}

impl PrintWriterCallback for StreamingPrint {
    fn stdout_write(&mut self, output: Cow<'_, str>) -> Result<(), MontyException> {
        self.buf.push_str(&output);
        Ok(())
    }

    fn stdout_push(&mut self, end: char) -> Result<(), MontyException> {
        self.buf.push(end);
        if end == '\n' {
            self.flush();
        }
        Ok(())
    }
}
