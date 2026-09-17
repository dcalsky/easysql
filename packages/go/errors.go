package easysql

import (
	"errors"
	"fmt"
)

type Status int32

const (
	StatusSuccess         Status = 0
	StatusInvalidArgument Status = 1
	StatusParseError      Status = 2
	StatusUnsupported     Status = 3
	StatusInternalError   Status = 4
	StatusPanic           Status = 99
)

var (
	ErrClosed          = errors.New("easysql: client is closed")
	ErrInvalidArgument = &Error{Status: StatusInvalidArgument}
	ErrParse           = &Error{Status: StatusParseError}
	ErrUnsupported     = &Error{Status: StatusUnsupported}
	ErrInternal        = &Error{Status: StatusInternalError}
	ErrPanic           = &Error{Status: StatusPanic}
)

type Error struct {
	Operation string
	Status    Status
	Message   string
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return fmt.Sprintf("easysql %s failed with status %d", e.Operation, e.Status)
	}
	return fmt.Sprintf("easysql %s failed with status %d: %s", e.Operation, e.Status, e.Message)
}

func (e *Error) Is(target error) bool {
	want, ok := target.(*Error)
	if !ok {
		return false
	}
	if want.Status != StatusSuccess && e.Status != want.Status {
		return false
	}
	return want.Operation == "" || e.Operation == want.Operation
}
