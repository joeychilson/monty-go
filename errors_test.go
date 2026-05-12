package monty

import (
	"errors"
	"testing"

	"github.com/joeychilson/monty/internal/ffi"
)

func TestNormalizeErrorConvertsFFIError(t *testing.T) {
	err := normalizeError(&ffi.Error{
		Type:    "RuntimeError",
		Message: "boom",
		Display: "RuntimeError: boom",
	})
	montyErr, ok := errors.AsType[*Error](err)
	if !ok {
		t.Fatalf("error = %T, want *Error", err)
	}
	if montyErr.Error() != "RuntimeError: boom" {
		t.Fatalf("display = %q", montyErr.Error())
	}
}
