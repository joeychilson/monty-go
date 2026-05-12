package monty

import (
	"testing"
	"time"
)

func TestLimitsWithContextDeadlineTightensDuration(t *testing.T) {
	base := &Limits{MaxDuration: time.Hour}
	limits := limitsWithContextDeadline(base, time.Now().Add(10*time.Millisecond))
	if limits == base {
		t.Fatal("limitsWithContextDeadline mutated the input")
	}
	if limits.MaxDuration <= 0 || limits.MaxDuration >= time.Hour {
		t.Fatalf("MaxDuration = %s, want positive duration shorter than base", limits.MaxDuration)
	}
}

func TestLimitsWithPastDeadlineUsesPositiveDuration(t *testing.T) {
	limits := limitsWithContextDeadline(nil, time.Now().Add(-time.Second))
	if limits.MaxDuration <= 0 {
		t.Fatalf("MaxDuration = %s, want positive duration", limits.MaxDuration)
	}
}
