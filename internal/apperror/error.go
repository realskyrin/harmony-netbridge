// Package apperror defines stable, user-facing error codes for the CLI.
package apperror

import (
	"errors"
	"fmt"
)

// Code is a stable error identifier that scripts may safely inspect.
type Code string

const (
	CodeHDCNotFound        Code = "HDC_NOT_FOUND"
	CodeNoDevice           Code = "NO_DEVICE"
	CodeDeviceOffline      Code = "DEVICE_OFFLINE"
	CodeMultipleDevices    Code = "MULTIPLE_DEVICES"
	CodeDeviceNotFound     Code = "DEVICE_NOT_FOUND"
	CodePortConflict       Code = "PORT_CONFLICT"
	CodeRPortFailed        Code = "RPORT_FAILED"
	CodeHandshakeTimeout   Code = "HANDSHAKE_TIMEOUT"
	CodeVersionUnsupported Code = "VERSION_UNSUPPORTED"
	CodeAppDisconnected    Code = "APP_DISCONNECTED"
	CodeDaemonRunning      Code = "DAEMON_ALREADY_RUNNING"
	CodeDaemonUnavailable  Code = "DAEMON_UNAVAILABLE"
	CodeInternal           Code = "INTERNAL_ERROR"
)

// Error carries a stable code, a safe user-facing message, and an optional cause.
type Error struct {
	Code    Code
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
}

// Unwrap exposes the diagnostic cause without changing the public code.
func (e *Error) Unwrap() error { return e.Err }

// New constructs an application error without a wrapped cause.
func New(code Code, message string) error {
	return &Error{Code: code, Message: message}
}

// Wrap constructs an application error with a diagnostic cause.
func Wrap(code Code, message string, err error) error {
	return &Error{Code: code, Message: message, Err: err}
}

// Details returns a stable code and safe message for any error.
func Details(err error) (Code, string) {
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr.Code, appErr.Message
	}
	return CodeInternal, "An unexpected error occurred. Check the HarmonyNetBridge log for details."
}
