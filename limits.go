package monty

import (
	"time"

	"github.com/joeychilson/monty/internal/ffi"
)

// Limits constrains CPU time, allocation count, memory, and recursion depth for a run.
type Limits struct {
	// MaxAllocations stops execution after this many Monty allocations.
	MaxAllocations int
	// MaxDuration stops execution after the duration has elapsed.
	MaxDuration time.Duration
	// MaxMemory stops execution after Monty reports this many bytes of memory use.
	MaxMemory int
	// GCInterval controls how often Monty checks memory pressure.
	GCInterval int
	// MaxRecursionDepth caps Python call recursion depth.
	MaxRecursionDepth int
	// DisableRecursionLimit disables Monty's recursion-depth guard.
	DisableRecursionLimit bool
}

func (l Limits) ffi() *ffi.Limits {
	var limits ffi.Limits
	if l.MaxAllocations > 0 {
		limits.MaxAllocationsSet = 1
		limits.MaxAllocations = uintptr(l.MaxAllocations)
	}
	if l.MaxDuration > 0 {
		limits.MaxDurationNanosSet = 1
		limits.MaxDurationNanos = uint64(l.MaxDuration)
	}
	if l.MaxMemory > 0 {
		limits.MaxMemorySet = 1
		limits.MaxMemory = uintptr(l.MaxMemory)
	}
	if l.GCInterval > 0 {
		limits.GCIntervalSet = 1
		limits.GCInterval = uintptr(l.GCInterval)
	}
	if l.MaxRecursionDepth > 0 {
		limits.MaxRecursionDepthSet = 1
		limits.MaxRecursionDepth = uintptr(l.MaxRecursionDepth)
	}
	if l.DisableRecursionLimit {
		limits.DisableRecursionLimit = 1
	}
	return &limits
}

func limitsWithContextDeadline(base *Limits, deadline time.Time) *Limits {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		remaining = time.Nanosecond
	}
	if base == nil {
		return &Limits{MaxDuration: remaining}
	}
	limits := *base
	if limits.MaxDuration <= 0 || remaining < limits.MaxDuration {
		limits.MaxDuration = remaining
	}
	return &limits
}
