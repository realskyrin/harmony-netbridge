// Package logging provides structured, size-bounded daemon logs.
package logging

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

const DefaultMaxBytes int64 = 5 * 1024 * 1024

// RotatingWriter keeps the active log plus one previous file.
type RotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	file     *os.File
	size     int64
}

// New creates a JSON slog logger and its owned writer.
func New(path string, maxBytes int64) (*slog.Logger, *RotatingWriter, error) {
	writer, err := OpenRotatingWriter(path, maxBytes)
	if err != nil {
		return nil, nil, err
	}
	handler := slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(handler), writer, nil
}

// OpenRotatingWriter opens or creates the active log.
func OpenRotatingWriter(path string, maxBytes int64) (*RotatingWriter, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	writer := &RotatingWriter{path: filepath.Clean(path), maxBytes: maxBytes}
	if err := writer.open(); err != nil {
		return nil, err
	}
	return writer, nil
}

func (w *RotatingWriter) open() error {
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	if err := os.Chmod(w.path, 0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("protect log file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("stat log file: %w", err)
	}
	w.file = file
	w.size = info.Size()
	return nil
}

func (w *RotatingWriter) Write(payload []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	if w.size > 0 && w.size+int64(len(payload)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	written, err := w.file.Write(payload)
	w.size += int64(written)
	return written, err
}

func (w *RotatingWriter) rotate() error {
	closeError := w.file.Close()
	w.file = nil
	if closeError != nil {
		return w.reopenAfterRotationError(fmt.Errorf("close log before rotation: %w", closeError))
	}
	previous := w.path + ".1"
	if err := os.Remove(previous); err != nil && !os.IsNotExist(err) {
		return w.reopenAfterRotationError(fmt.Errorf("remove previous log: %w", err))
	}
	if err := os.Rename(w.path, previous); err != nil && !os.IsNotExist(err) {
		return w.reopenAfterRotationError(fmt.Errorf("rotate log: %w", err))
	}
	return w.open()
}

func (w *RotatingWriter) reopenAfterRotationError(rotationError error) error {
	if reopenError := w.open(); reopenError != nil {
		return fmt.Errorf("%w; reopen active log: %v", rotationError, reopenError)
	}
	return rotationError
}

// Close releases the active log file.
func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
