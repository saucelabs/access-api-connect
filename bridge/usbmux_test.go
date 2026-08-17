package bridge

import (
	"bytes"
	"testing"
)

func TestUsbmuxMessageRoundTrip(t *testing.T) {
	body, err := encodeUsbmuxMessage(1, 8, 42, map[string]interface{}{
		"MessageType": "ListDevices",
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	msg, rest, ok, err := tryDecodeUsbmuxMessage(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !ok {
		t.Fatalf("expected complete message")
	}
	if len(rest) != 0 {
		t.Errorf("rest = %d bytes, want 0", len(rest))
	}
	if msg.Version != 1 || msg.Type != 8 || msg.Tag != 42 {
		t.Errorf("header = (v=%d t=%d tag=%d), want (1, 8, 42)", msg.Version, msg.Type, msg.Tag)
	}
	if mt, _ := msg.Payload["MessageType"].(string); mt != "ListDevices" {
		t.Errorf("MessageType = %q, want %q", mt, "ListDevices")
	}
}

func TestTryDecodeShortHeader(t *testing.T) {
	// Less than the 16-byte usbmux header.
	msg, rest, ok, err := tryDecodeUsbmuxMessage([]byte{0x01, 0x02, 0x03})
	if err != nil || ok || msg != nil {
		t.Fatalf("expected (nil, _, false, nil) for short header, got msg=%v ok=%v err=%v", msg, ok, err)
	}
	if len(rest) != 3 {
		t.Errorf("rest len = %d, want 3", len(rest))
	}
}

func TestTryDecodePartialBody(t *testing.T) {
	full, err := encodeUsbmuxMessage(1, 8, 1, map[string]interface{}{"MessageType": "ListDevices"})
	if err != nil {
		t.Fatal(err)
	}
	// Truncate before the end of the body.
	partial := full[:len(full)-5]
	msg, _, ok, err := tryDecodeUsbmuxMessage(partial)
	if err != nil || ok || msg != nil {
		t.Fatalf("expected partial decode to return ok=false, got msg=%v ok=%v err=%v", msg, ok, err)
	}
}

func TestTryDecodeMultipleMessages(t *testing.T) {
	a, _ := encodeUsbmuxMessage(1, 8, 1, map[string]interface{}{"MessageType": "ListDevices"})
	b, _ := encodeUsbmuxMessage(1, 8, 2, map[string]interface{}{"MessageType": "Listen"})
	buf := append(append([]byte{}, a...), b...)

	msg, rest, ok, err := tryDecodeUsbmuxMessage(buf)
	if err != nil || !ok {
		t.Fatalf("first decode failed: ok=%v err=%v", ok, err)
	}
	if msg.Tag != 1 {
		t.Errorf("first tag = %d, want 1", msg.Tag)
	}
	if !bytes.Equal(rest, b) {
		t.Errorf("rest should equal the second message bytes")
	}

	msg, rest, ok, err = tryDecodeUsbmuxMessage(rest)
	if err != nil || !ok {
		t.Fatalf("second decode failed: ok=%v err=%v", ok, err)
	}
	if msg.Tag != 2 {
		t.Errorf("second tag = %d, want 2", msg.Tag)
	}
	if len(rest) != 0 {
		t.Errorf("rest after second decode = %d bytes, want 0", len(rest))
	}
}
