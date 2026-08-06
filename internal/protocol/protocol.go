// Package protocol implements the HNB/1 framed transport shared with the Harmony app.
package protocol

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
)

const (
	HeaderSize        = 16
	CurrentVersion    = uint8(1)
	MaxControlPayload = 16 * 1024
	MaxPacketPayload  = 65_535
)

var magic = [4]byte{'H', 'N', 'B', '1'}

// Type identifies an HNB/1 frame.
type Type uint8

const (
	TypeHello       Type = 0x01
	TypeHelloAck    Type = 0x02
	TypeError       Type = 0x03
	TypeStopRequest Type = 0x04
	TypeStopAck     Type = 0x05

	TypeDataHello Type = 0x10
	TypeDataAck   Type = 0x11
	TypeIPPacket  Type = 0x20

	TypePing Type = 0x30
	TypePong Type = 0x31
)

// Frame is one decoded HNB/1 message.
type Frame struct {
	Type     Type
	Flags    uint16
	Sequence uint32
	Payload  []byte
}

// ProtocolError reports a malformed or unsupported peer frame.
type ProtocolError struct {
	Code    string
	Message string
}

func (e *ProtocolError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// Hello is sent by a Harmony control connection.
type Hello struct {
	Role              string   `json:"role"`
	Mode              string   `json:"mode"`
	AppVersion        string   `json:"appVersion"`
	SupportedVersions []int    `json:"supportedVersions"`
	Capabilities      []string `json:"capabilities"`
	Message           string   `json:"message"`
}

// HelloAck selects HNB/1 and proves the Mac received hello.
type HelloAck struct {
	SelectedVersion uint8    `json:"selectedVersion"`
	SessionToken    string   `json:"sessionToken"`
	Capabilities    []string `json:"capabilities"`
	Message         string   `json:"message"`
}

// ErrorPayload is safe to display on either peer.
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Fatal   bool   `json:"fatal"`
}

// StopRequest asks the peer to close the current bridge session.
type StopRequest struct {
	Reason string `json:"reason"`
}

// NewSessionToken returns 16 random bytes encoded as 32 lowercase hex characters.
func NewSessionToken() (string, error) {
	random := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, random); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return hex.EncodeToString(random), nil
}

// MarshalJSONPayload encodes a control payload.
func MarshalJSONPayload(value any) ([]byte, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode control payload: %w", err)
	}
	if len(payload) > MaxControlPayload {
		return nil, &ProtocolError{Code: "PAYLOAD_TOO_LARGE", Message: "control payload exceeds 16 KiB"}
	}
	return payload, nil
}

// UnmarshalJSONPayload decodes a control payload.
func UnmarshalJSONPayload(payload []byte, value any) error {
	if len(payload) > MaxControlPayload {
		return &ProtocolError{Code: "PAYLOAD_TOO_LARGE", Message: "control payload exceeds 16 KiB"}
	}
	if err := json.Unmarshal(payload, value); err != nil {
		return &ProtocolError{Code: "INVALID_JSON", Message: "control payload is not valid JSON"}
	}
	return nil
}

// SupportsVersion reports whether versions contains the current protocol version.
func SupportsVersion(versions []int) bool {
	for _, candidate := range versions {
		if candidate == int(CurrentVersion) {
			return true
		}
	}
	return false
}

// WriteFrame writes one complete HNB/1 frame.
func WriteFrame(writer io.Writer, frame Frame) error {
	if err := validateFrame(frame); err != nil {
		return err
	}
	header := make([]byte, HeaderSize)
	copy(header[0:4], magic[:])
	header[4] = CurrentVersion
	header[5] = byte(frame.Type)
	binary.BigEndian.PutUint16(header[6:8], frame.Flags)
	binary.BigEndian.PutUint32(header[8:12], uint32(len(frame.Payload)))
	binary.BigEndian.PutUint32(header[12:16], frame.Sequence)
	if err := writeAll(writer, header); err != nil {
		return fmt.Errorf("write HNB header: %w", err)
	}
	if err := writeAll(writer, frame.Payload); err != nil {
		return fmt.Errorf("write HNB payload: %w", err)
	}
	return nil
}

// ReadFrame reads one complete HNB/1 frame and validates its header and length.
func ReadFrame(reader io.Reader) (Frame, error) {
	header := make([]byte, HeaderSize)
	if _, err := io.ReadFull(reader, header); err != nil {
		return Frame{}, err
	}
	if string(header[0:4]) != string(magic[:]) {
		return Frame{}, &ProtocolError{Code: "INVALID_MAGIC", Message: "frame magic is not HNB1"}
	}
	if header[4] != CurrentVersion {
		return Frame{}, &ProtocolError{Code: "VERSION_UNSUPPORTED", Message: fmt.Sprintf("unsupported HNB version %d", header[4])}
	}
	frame := Frame{
		Type:     Type(header[5]),
		Flags:    binary.BigEndian.Uint16(header[6:8]),
		Sequence: binary.BigEndian.Uint32(header[12:16]),
	}
	payloadLength := binary.BigEndian.Uint32(header[8:12])
	if err := validateHeader(frame.Type, frame.Flags, frame.Sequence, payloadLength); err != nil {
		return Frame{}, err
	}
	frame.Payload = make([]byte, payloadLength)
	if _, err := io.ReadFull(reader, frame.Payload); err != nil {
		return Frame{}, err
	}
	if err := validateFrame(frame); err != nil {
		return Frame{}, err
	}
	return frame, nil
}

func validateFrame(frame Frame) error {
	return validateHeader(frame.Type, frame.Flags, frame.Sequence, uint32(len(frame.Payload)))
}

func validateHeader(frameType Type, flags uint16, sequence uint32, payloadLength uint32) error {
	if !knownType(frameType) {
		return &ProtocolError{Code: "UNKNOWN_FRAME_TYPE", Message: fmt.Sprintf("unknown frame type 0x%02x", uint8(frameType))}
	}
	if flags != 0 {
		return &ProtocolError{Code: "INVALID_FLAGS", Message: "HNB/1 flags must be zero"}
	}
	if sequence == 0 {
		return &ProtocolError{Code: "INVALID_SEQUENCE", Message: "HNB/1 sequence zero is reserved"}
	}
	return validateLength(frameType, payloadLength)
}

func validateLength(frameType Type, length uint32) error {
	limit := uint32(MaxControlPayload)
	if frameType == TypeIPPacket {
		limit = MaxPacketPayload
	}
	if length > limit {
		return &ProtocolError{Code: "PAYLOAD_TOO_LARGE", Message: fmt.Sprintf("frame payload %d exceeds limit %d", length, limit)}
	}
	return nil
}

func knownType(frameType Type) bool {
	switch frameType {
	case TypeHello, TypeHelloAck, TypeError, TypeStopRequest, TypeStopAck,
		TypeDataHello, TypeDataAck, TypeIPPacket, TypePing, TypePong:
		return true
	default:
		return false
	}
}

func writeAll(writer io.Writer, payload []byte) error {
	for len(payload) > 0 {
		written, err := writer.Write(payload)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		payload = payload[written:]
	}
	return nil
}
