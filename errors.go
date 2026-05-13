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
	switch {
	case e.Display != "":
		return e.Display
	case e.Message == "":
		return e.Type
	default:
		return e.Type + ": " + e.Message
	}
}

func normalizeError(err error) error {
	if ffiErr, ok := errors.AsType[*ffi.Error](err); ok {
		return &Error{Type: ffiErr.Type, Message: ffiErr.Message, Display: ffiErr.Display}
	}
	return err
}

func exceptionFromError(err error) (string, string) {
	const fallback = "RuntimeError"
	if err == nil {
		return fallback, ""
	}
	if me, ok := errors.AsType[*Error](err); ok && me.Type != "" {
		message := me.Message
		if message == "" {
			message = me.Display
		}
		return me.Type, message
	}
	return fallback, err.Error()
}
