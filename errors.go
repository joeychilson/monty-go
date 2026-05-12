package monty

import (
	"errors"

	"github.com/joeychilson/monty/internal/ffi"
)

// Error describes an exception reported by Monty.
type Error struct {
	// Type is the Python exception type, such as "NameError" or "RuntimeError".
	Type string
	// Message is the exception message without the type prefix.
	Message string
	// Display is the fully formatted exception text.
	Display string
}

// Error returns the formatted Monty exception text.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Display != "" {
		return e.Display
	}
	if e.Message == "" {
		return e.Type
	}
	return e.Type + ": " + e.Message
}

func normalizeError(err error) error {
	if err == nil {
		return nil
	}
	if ffiErr, ok := errors.AsType[*ffi.Error](err); ok {
		return &Error{
			Type:    ffiErr.Type,
			Message: ffiErr.Message,
			Display: ffiErr.Display,
		}
	}
	return err
}

func joinErrors(primary, secondary error) error {
	if primary == nil {
		return secondary
	}
	if secondary == nil {
		return primary
	}
	return errors.Join(primary, secondary)
}

func exceptionFromError(err error) (string, string) {
	const fallbackType = "RuntimeError"
	if err == nil {
		return fallbackType, ""
	}
	if montyErr, ok := errors.AsType[*Error](err); ok && montyErr.Type != "" {
		message := montyErr.Message
		if message == "" {
			message = montyErr.Display
		}
		if message == "" {
			message = montyErr.Error()
		}
		return montyErr.Type, message
	}
	return fallbackType, err.Error()
}
