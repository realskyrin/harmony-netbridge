// Package control implements the local one-request-per-connection protocol used
// by the CLI to query and stop the background daemon.
package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/realskyrin/harmony-netbridge/internal/apperror"
	"github.com/realskyrin/harmony-netbridge/internal/state"
)

const (
	maxMessageBytes = 64 * 1024
	defaultTimeout  = 5 * time.Second
)

// Command identifies a local daemon operation.
type Command string

const (
	CommandStatus Command = "status"
	CommandStop   Command = "stop"
)

// Request is sent as one newline-terminated JSON object.
type Request struct {
	Command Command `json:"command"`
}

// Response is returned as one newline-terminated JSON object.
type Response struct {
	OK        bool           `json:"ok"`
	State     state.Snapshot `json:"state"`
	ErrorCode apperror.Code  `json:"errorCode,omitempty"`
	Error     string         `json:"error,omitempty"`
}

// Call performs one local control request. The Unix socket is the liveness
// authority; callers must not infer daemon health from a PID or state file.
func Call(ctx context.Context, socketPath string, request Request) (Response, error) {
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return Response{}, fmt.Errorf("connect daemon control socket: %w", err)
	}
	defer connection.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	} else {
		_ = connection.SetDeadline(time.Now().Add(defaultTimeout))
	}

	if err := Write(connection, request); err != nil {
		return Response{}, fmt.Errorf("send daemon control request: %w", err)
	}
	var response Response
	if err := Read(connection, &response); err != nil {
		return Response{}, fmt.Errorf("read daemon control response: %w", err)
	}
	return response, nil
}

// Read decodes one bounded newline-terminated JSON message.
func Read(reader io.Reader, value any) error {
	limited := io.LimitReader(reader, maxMessageBytes+1)
	buffered := bufio.NewReaderSize(limited, 4096)
	payload, err := buffered.ReadBytes('\n')
	if len(payload) > maxMessageBytes {
		return fmt.Errorf("control message exceeds %d bytes", maxMessageBytes)
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(payload, value); err != nil {
		return fmt.Errorf("decode control message: %w", err)
	}
	return nil
}

// Write encodes one bounded newline-terminated JSON message.
func Write(writer io.Writer, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode control message: %w", err)
	}
	if len(payload)+1 > maxMessageBytes {
		return fmt.Errorf("control message exceeds %d bytes", maxMessageBytes)
	}
	payload = append(payload, '\n')
	for len(payload) > 0 {
		written, writeErr := writer.Write(payload)
		if writeErr != nil {
			return writeErr
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		payload = payload[written:]
	}
	return nil
}

// IsUnavailable reports errors that mean no live daemon control socket could
// be reached. It intentionally does not inspect the persisted state file.
func IsUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var operationError *net.OpError
	return errors.As(err, &operationError)
}
