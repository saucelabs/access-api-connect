package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeBridge satisfies the localBridge interface but records what the
// handler does instead of actually talking to a real WebSocket.
type fakeBridge struct {
	mu sync.Mutex

	props    map[string]interface{}
	devID    int
	next     uint32
	opened   []uint32
	sent     []sentFrame
	closed   []uint32
	handlers map[uint32]struct {
		onData  dataHandler
		onClose closeHandler
	}
}

type sentFrame struct {
	cid     uint32
	payload []byte
}

func newFakeBridge(props map[string]interface{}, devID int) *fakeBridge {
	return &fakeBridge{
		props: props,
		devID: devID,
		handlers: map[uint32]struct {
			onData  dataHandler
			onClose closeHandler
		}{},
	}
}

func (b *fakeBridge) AllocChannel() uint32 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	b.opened = append(b.opened, b.next)
	return b.next
}

func (b *fakeBridge) RegisterHandlers(cid uint32, onData dataHandler, onClose closeHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[cid] = struct {
		onData  dataHandler
		onClose closeHandler
	}{onData, onClose}
}

func (b *fakeBridge) SendToChannel(cid uint32, payload []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = append(b.sent, sentFrame{cid: cid, payload: append([]byte(nil), payload...)})
	return nil
}

func (b *fakeBridge) CloseChannel(cid uint32) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = append(b.closed, cid)
	return nil
}

func (b *fakeBridge) DeviceProperties() map[string]interface{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]interface{}, len(b.props))
	for k, v := range b.props {
		out[k] = v
	}
	return out
}

func (b *fakeBridge) LocalDeviceID() int { return b.devID }

func (b *fakeBridge) snapshot() ([]uint32, []sentFrame, []uint32) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]uint32(nil), b.opened...),
		append([]sentFrame(nil), b.sent...),
		append([]uint32(nil), b.closed...)
}

// fakeConn satisfies net.Conn with deterministic, in-memory semantics.
// Reads come from a pre-populated buffer; once that buffer is drained
// every subsequent Read returns io.EOF (so the handler's loop exits
// cleanly). Writes accumulate in `out` for the test to inspect.
type fakeConn struct {
	mu     sync.Mutex
	in     *bytes.Buffer
	out    bytes.Buffer
	closed bool
}

func newFakeConn(script []byte) *fakeConn {
	return &fakeConn{in: bytes.NewBuffer(script)}
}

func (c *fakeConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, io.EOF
	}
	if c.in.Len() == 0 {
		return 0, io.EOF
	}
	return c.in.Read(b)
}

func (c *fakeConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, errors.New("closed")
	}
	return c.out.Write(b)
}

func (c *fakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *fakeConn) bytesWritten() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.out.Bytes()...)
}

func (c *fakeConn) LocalAddr() net.Addr                { return &net.UnixAddr{Net: "unix"} }
func (c *fakeConn) RemoteAddr() net.Addr               { return &net.UnixAddr{Net: "unix"} }
func (c *fakeConn) SetDeadline(time.Time) error        { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error    { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error   { return nil }

// drive runs a LocalUsbmuxHandler against `script` and returns whatever
// the handler wrote back to its local conn.
func drive(t *testing.T, bridge localBridge, script []byte) []byte {
	t.Helper()
	conn := newFakeConn(script)
	h := NewLocalUsbmuxHandler(bridge, conn)

	done := make(chan struct{})
	go func() {
		h.Run()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not exit within 2s")
	}
	return conn.bytesWritten()
}

func encodePlist(t *testing.T, messageType string, extras map[string]interface{}) []byte {
	t.Helper()
	payload := map[string]interface{}{"MessageType": messageType}
	for k, v := range extras {
		payload[k] = v
	}
	b, err := encodeUsbmuxMessage(1, 8, 1, payload)
	if err != nil {
		t.Fatalf("encodePlist: %v", err)
	}
	return b
}

func TestListDevicesAnsweredLocally(t *testing.T) {
	br := newFakeBridge(map[string]interface{}{"SerialNumber": "abc123", "ProductType": "iPhone15,3"}, 1)
	out := drive(t, br, encodePlist(t, "ListDevices", nil))

	opened, sent, _ := br.snapshot()
	if len(opened) != 0 {
		t.Errorf("ListDevices should not allocate any upstream channel, got %v", opened)
	}
	if len(sent) != 0 {
		t.Errorf("ListDevices should not send upstream, got %d frames", len(sent))
	}

	msg, _, ok, err := tryDecodeUsbmuxMessage(out)
	if err != nil || !ok {
		t.Fatalf("decode local response: ok=%v err=%v", ok, err)
	}
	devices, _ := msg.Payload["DeviceList"].([]interface{})
	if len(devices) != 1 {
		t.Fatalf("DeviceList len = %d, want 1", len(devices))
	}
	dev, _ := devices[0].(map[string]interface{})
	props, _ := dev["Properties"].(map[string]interface{})
	// howett.net/plist may unmarshal numbers as uint64 or int64 depending
	// on the value — accept anything that's "1".
	got := props["DeviceID"]
	if v, ok := got.(uint64); ok && v == 1 {
		return
	}
	if v, ok := got.(int64); ok && v == 1 {
		return
	}
	if v, ok := got.(int); ok && v == 1 {
		return
	}
	t.Errorf("Properties.DeviceID = %v (%T), want 1", got, got)
}

func TestListenAnsweredLocally(t *testing.T) {
	br := newFakeBridge(map[string]interface{}{"SerialNumber": "abc", "ProductType": "iPhone15,3"}, 1)
	out := drive(t, br, encodePlist(t, "Listen", nil))

	// Should produce two PLIST messages: Result Number=0, then Attached.
	first, rest, ok, err := tryDecodeUsbmuxMessage(out)
	if err != nil || !ok {
		t.Fatalf("decode first: ok=%v err=%v", ok, err)
	}
	if mt, _ := first.Payload["MessageType"].(string); mt != "Result" {
		t.Errorf("first MessageType = %q, want Result", mt)
	}

	second, _, ok, err := tryDecodeUsbmuxMessage(rest)
	if err != nil || !ok {
		t.Fatalf("decode second: ok=%v err=%v", ok, err)
	}
	if mt, _ := second.Payload["MessageType"].(string); mt != "Attached" {
		t.Errorf("second MessageType = %q, want Attached", mt)
	}
}

func TestSavePairRecordAnsweredLocally(t *testing.T) {
	br := newFakeBridge(map[string]interface{}{"SerialNumber": "abc"}, 1)
	out := drive(t, br, encodePlist(t, "SavePairRecord", nil))

	opened, sent, _ := br.snapshot()
	if len(opened) != 0 || len(sent) != 0 {
		t.Errorf("SavePairRecord should be local-only; opened=%v sent=%d", opened, len(sent))
	}
	msg, _, ok, err := tryDecodeUsbmuxMessage(out)
	if err != nil || !ok {
		t.Fatalf("decode: ok=%v err=%v", ok, err)
	}
	if mt, _ := msg.Payload["MessageType"].(string); mt != "Result" {
		t.Errorf("MessageType = %q, want Result", mt)
	}
}

func TestConnectSwitchesToPassthrough(t *testing.T) {
	br := newFakeBridge(map[string]interface{}{"SerialNumber": "abc"}, 1)
	plist := encodePlist(t, "Connect", map[string]interface{}{
		"DeviceID":   1,
		"PortNumber": 62078,
	})
	rawTrailing := []byte{0x00, 0x01, 0x02, 0x03}
	drive(t, br, append(append([]byte(nil), plist...), rawTrailing...))

	opened, sent, closed := br.snapshot()
	if len(opened) != 1 || opened[0] != 1 {
		t.Fatalf("expected one allocated channel (id=1), got %v", opened)
	}
	if len(sent) != 2 {
		t.Fatalf("expected 2 upstream sends (PLIST + raw), got %d", len(sent))
	}
	if !bytes.Equal(sent[0].payload, plist) {
		t.Errorf("first upstream send should be the Connect PLIST")
	}
	if !bytes.Equal(sent[1].payload, rawTrailing) {
		t.Errorf("second upstream send should be the trailing raw bytes")
	}
	if len(closed) != 1 || closed[0] != 1 {
		t.Errorf("channel should be closed on local EOF, got closed=%v", closed)
	}
}
