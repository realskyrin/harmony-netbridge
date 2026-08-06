package control

import (
	"bytes"
	"strings"
	"testing"
)

func TestReadWriteRoundTrip(t *testing.T) {
	var buffer bytes.Buffer
	want := Request{Command: CommandStatus}
	if err := Write(&buffer, want); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	var got Request
	if err := Read(&buffer, &got); err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}
}

func TestReadRejectsOversizedMessage(t *testing.T) {
	payload := strings.Repeat("x", maxMessageBytes) + "\n"
	var got Request
	if err := Read(strings.NewReader(payload), &got); err == nil {
		t.Fatal("Read() error = nil, want oversized message error")
	}
}
