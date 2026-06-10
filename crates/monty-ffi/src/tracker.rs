//! Resource tracker with host-driven cancellation.
//!
//! Upstream Monty has no interrupt hook; the blessed seam is the
//! `ResourceTracker` trait, whose `check_time` runs at statement boundaries.
//! `GoTracker` wraps `LimitedTracker` and additionally observes a shared
//! atomic flag so a Go `context` cancellation can abort a run mid-flight.

use std::cell::Cell;
use std::sync::{
    Arc,
    atomic::{AtomicBool, Ordering},
};
use std::time::{Duration, Instant};

use monty::{
    ExcType, LimitedTracker, MontyException, ResourceError, ResourceLimits, ResourceTracker,
};
use serde::{Deserialize, Serialize};

use crate::MgCancelToken;

/// How many `check_time` calls to skip between `Instant::now()` reads for the
/// snippet deadline; mirrors upstream `LimitedTracker`'s rate limiting.
const DEADLINE_CHECK_INTERVAL: u16 = 10;

/// `LimitedTracker` plus host-driven execution controls: a cancellation flag
/// and a per-snippet deadline (REPL feeds reset it for every snippet, so one
/// snippet's budget never leaks into the next).
///
/// Both extras are `#[serde(skip)]`, so `GoTracker` serializes byte-for-byte
/// like `LimitedTracker` and snapshots restored in another process simply
/// lose the (process-local) controls.
#[derive(Debug, Serialize, Deserialize)]
pub struct GoTracker {
    inner: LimitedTracker,
    #[serde(skip)]
    cancel: Option<Arc<AtomicBool>>,
    #[serde(skip)]
    deadline: Option<(Instant, Duration)>,
    #[serde(skip)]
    deadline_counter: Cell<u16>,
}

impl GoTracker {
    pub(crate) fn new(limits: ResourceLimits, cancel: Option<Arc<AtomicBool>>) -> Self {
        Self {
            inner: LimitedTracker::new(limits),
            cancel,
            deadline: None,
            deadline_counter: Cell::new(0),
        }
    }

    pub(crate) fn set_cancel(&mut self, cancel: Option<Arc<AtomicBool>>) {
        self.cancel = cancel;
    }

    /// Sets (or clears) the budget for the next snippet.
    pub(crate) fn set_snippet_deadline(&mut self, budget: Option<Duration>) {
        self.deadline = budget.map(|budget| (Instant::now(), budget));
        self.deadline_counter.set(0);
    }

    fn check_cancelled(&self) -> Result<(), ResourceError> {
        if let Some(cancel) = &self.cancel
            && cancel.load(Ordering::Relaxed)
        {
            return Err(ResourceError::Exception(MontyException::new(
                ExcType::KeyboardInterrupt,
                Some("execution cancelled by host".to_owned()),
            )));
        }
        Ok(())
    }

    fn check_deadline(&self) -> Result<(), ResourceError> {
        if let Some((started, budget)) = self.deadline {
            self.deadline_counter.update(|c| c.wrapping_add(1));
            if self
                .deadline_counter
                .get()
                .is_multiple_of(DEADLINE_CHECK_INTERVAL)
            {
                let elapsed = started.elapsed();
                if elapsed > budget {
                    // Re-arm so the very next check re-detects the timeout even
                    // when a caller swallows this error.
                    self.deadline_counter
                        .set(DEADLINE_CHECK_INTERVAL.wrapping_sub(1));
                    return Err(ResourceError::Time {
                        limit: budget,
                        elapsed,
                    });
                }
            }
        }
        Ok(())
    }
}

impl ResourceTracker for GoTracker {
    #[inline]
    fn on_allocate(&self, get_size: impl FnOnce() -> usize) -> Result<(), ResourceError> {
        self.inner.on_allocate(get_size)
    }

    #[inline]
    fn on_free(&self, get_size: impl FnOnce() -> usize) {
        self.inner.on_free(get_size);
    }

    #[inline]
    fn check_time(&self) -> Result<(), ResourceError> {
        self.check_cancelled()?;
        self.check_deadline()?;
        self.inner.check_time()
    }

    #[inline]
    fn check_recursion_depth(&self, current_depth: usize) -> Result<(), ResourceError> {
        self.inner.check_recursion_depth(current_depth)
    }

    #[inline]
    fn check_large_result(&self, estimated_bytes: usize) -> Result<(), ResourceError> {
        self.inner.check_large_result(estimated_bytes)
    }

    #[inline]
    fn on_grow(&self, additional_bytes: usize) -> Result<(), ResourceError> {
        self.inner.on_grow(additional_bytes)
    }

    #[inline]
    fn gc_interval(&self) -> Option<usize> {
        self.inner.gc_interval()
    }
}

#[unsafe(no_mangle)]
pub extern "C" fn mg_cancel_token_new() -> *mut MgCancelToken {
    Box::into_raw(Box::new(MgCancelToken(Arc::new(AtomicBool::new(false)))))
}

/// Requests cancellation. Safe to call from any thread while another thread is
/// blocked inside a run/resume entry point using this token.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_cancel_token_cancel(token: *mut MgCancelToken) {
    if !token.is_null() {
        // SAFETY: token handles are allocated with Box::into_raw in this crate.
        unsafe { (*token).0.store(true, Ordering::Relaxed) };
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn mg_cancel_token_free(token: *mut MgCancelToken) {
    if !token.is_null() {
        // SAFETY: token handles are allocated with Box::into_raw in this crate.
        unsafe { drop(Box::from_raw(token)) };
    }
}
