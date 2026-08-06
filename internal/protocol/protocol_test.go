package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"testing"
)

func TestFrameRoundTripWithFragmentedReader(t *testing.T) {
	t.Parallel()
	payload, err := MarshalJSONPayload(Hello{Role: "control", Mode: "phase1", Message: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	want := Frame{Type: TypeHello, Sequence: 1, Payload: payload}
	var encoded bytes.Buffer
	if err := WriteFrame(&encoded, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrame(&chunkReader{reader: bytes.NewReader(encoded.Bytes()), max: 3})
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != want.Type || got.Sequence != want.Sequence || !bytes.Equal(got.Payload, want.Payload) {
		t.Fatalf("round trip mismatch: got %#v want %#v", got, want)
	}
}

func TestPhase1HelloMatchesSharedGoldenFrame(t *testing.T) {
	t.Parallel()
	type goldenFrame struct {
		PayloadUTF8 string `json:"payloadUtf8"`
		FrameHex    string `json:"frameHex"`
	}
	payload, err := os.ReadFile("../../testdata/hnb1-phase1-hello.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden goldenFrame
	if err := json.Unmarshal(payload, &golden); err != nil {
		t.Fatal(err)
	}
	helloPayload, err := MarshalJSONPayload(Hello{
		Role:              "control",
		Mode:              "phase1",
		AppVersion:        "development",
		SupportedVersions: []int{1},
		Capabilities:      []string{"control"},
		Message:           "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(helloPayload) != golden.PayloadUTF8 {
		t.Fatalf("HELLO payload = %q, want %q", helloPayload, golden.PayloadUTF8)
	}
	var encoded bytes.Buffer
	if err := WriteFrame(&encoded, Frame{Type: TypeHello, Sequence: 1, Payload: helloPayload}); err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(encoded.Bytes()); got != golden.FrameHex {
		t.Fatalf("HELLO frame = %s, want %s", got, golden.FrameHex)
	}
}

func TestReadCoalescedFrames(t *testing.T) {
	t.Parallel()
	var encoded bytes.Buffer
	for sequence, message := range []string{"hello", "world"} {
		payload, err := MarshalJSONPayload(map[string]string{"message": message})
		if err != nil {
			t.Fatal(err)
		}
		if err := WriteFrame(&encoded, Frame{Type: TypeHello, Sequence: uint32(sequence + 1), Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	for sequence := uint32(1); sequence <= 2; sequence++ {
		frame, err := ReadFrame(&encoded)
		if err != nil {
			t.Fatal(err)
		}
		if frame.Sequence != sequence {
			t.Fatalf("sequence = %d, want %d", frame.Sequence, sequence)
		}
	}
}

func TestReadFrameRejectsInvalidHeaders(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func([]byte)
		code   string
	}{
		{name: "magic", mutate: func(frame []byte) { frame[0] = 'X' }, code: "INVALID_MAGIC"},
		{name: "version", mutate: func(frame []byte) { frame[4] = 2 }, code: "VERSION_UNSUPPORTED"},
		{name: "flags", mutate: func(frame []byte) { frame[7] = 1 }, code: "INVALID_FLAGS"},
		{name: "sequence", mutate: func(frame []byte) { binary.BigEndian.PutUint32(frame[12:16], 0) }, code: "INVALID_SEQUENCE"},
		{name: "type", mutate: func(frame []byte) { frame[5] = 0xff }, code: "UNKNOWN_FRAME_TYPE"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var encoded bytes.Buffer
			if err := WriteFrame(&encoded, Frame{Type: TypeHello, Sequence: 1, Payload: []byte(`{}`)}); err != nil {
				t.Fatal(err)
			}
			data := encoded.Bytes()
			test.mutate(data)
			_, err := ReadFrame(bytes.NewReader(data))
			var protocolErr *ProtocolError
			if !errors.As(err, &protocolErr) || protocolErr.Code != test.code {
				t.Fatalf("error = %v, want protocol code %s", err, test.code)
			}
		})
	}
}

func TestReadFrameRejectsOversizedPayloadBeforeAllocation(t *testing.T) {
	t.Parallel()
	header := make([]byte, HeaderSize)
	copy(header[0:4], magic[:])
	header[4] = CurrentVersion
	header[5] = byte(TypeHello)
	binary.BigEndian.PutUint32(header[8:12], MaxControlPayload+1)
	binary.BigEndian.PutUint32(header[12:16], 1)
	_, err := ReadFrame(bytes.NewReader(header))
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Code != "PAYLOAD_TOO_LARGE" {
		t.Fatalf("error = %v, want PAYLOAD_TOO_LARGE", err)
	}
}

func TestReadFrameRejectsInvalidHeaderBeforeReadingPayload(t *testing.T) {
	t.Parallel()
	header := make([]byte, HeaderSize)
	copy(header[0:4], magic[:])
	header[4] = CurrentVersion
	header[5] = byte(TypeHello)
	binary.BigEndian.PutUint16(header[6:8], 1)
	binary.BigEndian.PutUint32(header[8:12], 128)
	binary.BigEndian.PutUint32(header[12:16], 1)
	_, err := ReadFrame(bytes.NewReader(header))
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Code != "INVALID_FLAGS" {
		t.Fatalf("error = %v, want INVALID_FLAGS before payload read", err)
	}
}

func TestReadFrameReportsTruncatedPayload(t *testing.T) {
	t.Parallel()
	var encoded bytes.Buffer
	if err := WriteFrame(&encoded, Frame{Type: TypeHello, Sequence: 1, Payload: []byte(`{"message":"hello"}`)}); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	_, err := ReadFrame(bytes.NewReader(data[:len(data)-1]))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error = %v, want unexpected EOF", err)
	}
}

func TestValidSessionTokenRequiresLowercaseHex(t *testing.T) {
	t.Parallel()
	if !ValidSessionToken("0123456789abcdef0123456789abcdef") {
		t.Fatal("ValidSessionToken rejected lowercase token")
	}
	for _, token := range []string{
		"0123456789ABCDEF0123456789ABCDEF",
		"0123456789abcdef",
		"0123456789abcdef0123456789abcdeg",
	} {
		if ValidSessionToken(token) {
			t.Fatalf("ValidSessionToken accepted %q", token)
		}
	}
}

func TestNewSessionTokenFormatAndUniqueness(t *testing.T) {
	t.Parallel()
	first, err := NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(first) {
		t.Fatalf("token has unexpected format: %q", first)
	}
	if first == second {
		t.Fatal("two generated session tokens are equal")
	}
}

func TestHelloAckCarriesNegotiatedMTU(t *testing.T) {
	payload, err := MarshalJSONPayload(HelloAck{
		SelectedVersion: CurrentVersion,
		SessionToken:    "0123456789abcdef0123456789abcdef",
		Capabilities:    []string{"control", "data", "heartbeat", "reconnect", "mtu"},
		MTU:             1280,
		Message:         "world",
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded HelloAck
	if err := UnmarshalJSONPayload(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.MTU != 1280 {
		t.Fatalf("decoded MTU = %d", decoded.MTU)
	}
}

type chunkReader struct {
	reader io.Reader
	max    int
}

func (r *chunkReader) Read(payload []byte) (int, error) {
	if len(payload) > r.max {
		payload = payload[:r.max]
	}
	return r.reader.Read(payload)
}
