package bridge

import (
	"bytes"
	"testing"
)

func TestEncodeFrameLayout(t *testing.T) {
	got := encodeFrame(opOpen, 0x12345678, []byte("hello"))
	want := []byte{
		0x00, 0x00, 0xBE, 0xEF, // magic (big-endian)
		0x00, 0x01, // opcode = OPEN
		0x12, 0x34, 0x56, 0x78, // channel id (big-endian)
		'h', 'e', 'l', 'l', 'o',
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("encodeFrame layout mismatch\n got: %x\nwant: %x", got, want)
	}
}

func TestEncodeFrameCloseHasNoPayload(t *testing.T) {
	got := encodeFrame(opClose, 7, nil)
	want := []byte{
		0x00, 0x00, 0xBE, 0xEF,
		0x00, 0x03, // CLOSE
		0x00, 0x00, 0x00, 0x07,
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("CLOSE frame mismatch\n got: %x\nwant: %x", got, want)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		opcode  uint16
		channel uint32
		payload []byte
	}{
		{"open with payload", opOpen, 1, []byte("hello world")},
		{"data large payload", opData, 42, bytes.Repeat([]byte{0xAB}, 4096)},
		{"close empty", opClose, 0xFFFFFFFE, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frame := encodeFrame(tc.opcode, tc.channel, tc.payload)
			op, cid, payload, err := decodeFrame(frame)
			if err != nil {
				t.Fatalf("decodeFrame: %v", err)
			}
			if op != tc.opcode {
				t.Errorf("opcode = %d, want %d", op, tc.opcode)
			}
			if cid != tc.channel {
				t.Errorf("channel = %d, want %d", cid, tc.channel)
			}
			if !bytes.Equal(payload, tc.payload) {
				t.Errorf("payload mismatch (len got=%d want=%d)", len(payload), len(tc.payload))
			}
		})
	}
}

func TestDecodeFrameTooShort(t *testing.T) {
	if _, _, _, err := decodeFrame([]byte{0x00, 0x00, 0xBE, 0xEF}); err == nil {
		t.Fatal("expected error for short frame")
	}
}

func TestDecodeFrameBadMagic(t *testing.T) {
	bad := []byte{
		0xDE, 0xAD, 0xBE, 0xEF, // wrong magic
		0x00, 0x01,
		0x00, 0x00, 0x00, 0x01,
	}
	if _, _, _, err := decodeFrame(bad); err == nil {
		t.Fatal("expected error for bad magic")
	}
}
