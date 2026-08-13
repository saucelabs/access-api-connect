package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// dataHandler runs for a DATA frame on a registered channel; closeHandler
// runs on CLOSE, and also when the bridge tears channels down before a
// reconnect (ResetForReconnect).
type dataHandler func([]byte) error
type closeHandler func() error

// Keepalive: we ping on our own cadence and arm a read deadline, so a
// silently dead link (sleep, NAT expiry, mid-path reset) fails the read in
// seconds instead of blocking until the OS gives up.
const (
	defaultPingInterval = 10 * time.Second // outbound ping cadence
	defaultPongTimeout  = 25 * time.Second // max silence before the link is dead
	defaultWriteWait    = 5 * time.Second  // control-frame write deadline
)

// DeviceBridge owns one multiplexed WebSocket and the per-channel routing
// tables for the iOS path. First send on a channel is OPEN, subsequent
// sends are DATA, and CLOSE goes out when the local end goes away.
//
// The bridge survives WebSocket failures: Reconnect dials a fresh
// connection for the same session, and ResetForReconnect drops all channel
// state from the previous connection.
type DeviceBridge struct {
	wsURL     string
	sessionID string
	authB64   string

	ws     *websocket.Conn
	sendMu sync.Mutex // serializes WriteMessage and guards the ws pointer

	chMu             sync.Mutex
	nextChannel      uint32
	channelFirstSend map[uint32]bool
	dataHandlers     map[uint32]dataHandler
	closeHandlers    map[uint32]closeHandler

	propsMu          sync.Mutex
	deviceProperties map[string]interface{}
	localDeviceID    int

	pingInterval time.Duration
	pongTimeout  time.Duration
	writeWait    time.Duration
}

// NewDeviceBridge constructs an unconnected bridge. Call Connect to dial.
func NewDeviceBridge(wsURL, sessionID, username, accessKey string) *DeviceBridge {
	return &DeviceBridge{
		wsURL:            wsURL,
		sessionID:        sessionID,
		authB64:          base64.StdEncoding.EncodeToString([]byte(username + ":" + accessKey)),
		nextChannel:      1,
		channelFirstSend: map[uint32]bool{},
		dataHandlers:     map[uint32]dataHandler{},
		closeHandlers:    map[uint32]closeHandler{},
		localDeviceID:    1,
		pingInterval:     defaultPingInterval,
		pongTimeout:      defaultPongTimeout,
		writeWait:        defaultWriteWait,
	}
}

// Connect dials the WebSocket using the supplied sessionId / basic-auth
// headers. The connection then services every channel until it dies or
// Close is called.
func (b *DeviceBridge) Connect(ctx context.Context) error {
	header := http.Header{}
	header.Set("sessionId", b.sessionID)
	header.Set("Authorization", "Basic "+b.authB64)

	dialer := *websocket.DefaultDialer
	dialer.ReadBufferSize = 4 * 1024 * 1024
	dialer.WriteBufferSize = 4 * 1024 * 1024

	ws, _, err := dialer.DialContext(ctx, b.wsURL, header)
	if err != nil {
		return err
	}
	b.sendMu.Lock()
	b.ws = ws
	b.sendMu.Unlock()
	return nil
}

// Reconnect drops whatever connection is left and dials a fresh one for
// the same session. Callers must invalidate old channel state first
// (ResetForReconnect); stale channels do not survive a new connection.
func (b *DeviceBridge) Reconnect(ctx context.Context) error {
	b.sendMu.Lock()
	if b.ws != nil {
		_ = b.ws.Close()
		b.ws = nil
	}
	b.sendMu.Unlock()
	return b.Connect(ctx)
}

// Close drops the underlying WebSocket. Safe to call multiple times.
func (b *DeviceBridge) Close() error {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	if b.ws == nil {
		return nil
	}
	err := b.ws.Close()
	b.ws = nil
	return err
}

func (b *DeviceBridge) currentWS() *websocket.Conn {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	return b.ws
}

// AllocChannel reserves a fresh channel id and marks it as needing OPEN
// on its first send.
func (b *DeviceBridge) AllocChannel() uint32 {
	b.chMu.Lock()
	defer b.chMu.Unlock()
	cid := b.nextChannel
	b.nextChannel++
	b.channelFirstSend[cid] = true
	return cid
}

// RegisterHandlers wires up the per-channel callbacks. The reader loop
// invokes onData for incoming DATA frames and onClose for CLOSE frames.
func (b *DeviceBridge) RegisterHandlers(cid uint32, onData dataHandler, onClose closeHandler) {
	b.chMu.Lock()
	defer b.chMu.Unlock()
	b.dataHandlers[cid] = onData
	b.closeHandlers[cid] = onClose
}

// SendToChannel emits one outgoing frame on cid (OPEN on the first call,
// DATA after). A channel that no longer exists (closed, or dropped by a
// reconnect) returns an error rather than silently re-OPENing a stale id.
func (b *DeviceBridge) SendToChannel(cid uint32, payload []byte) error {
	b.chMu.Lock()
	first, known := b.channelFirstSend[cid]
	if !known {
		b.chMu.Unlock()
		return fmt.Errorf("channel %d is not open (closed or connection re-established)", cid)
	}
	var opcode uint16 = opData
	if first {
		opcode = opOpen
	}
	b.channelFirstSend[cid] = false
	b.chMu.Unlock()

	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	if b.ws == nil {
		return errors.New("websocket not connected")
	}
	return b.ws.WriteMessage(websocket.BinaryMessage, encodeFrame(opcode, cid, payload))
}

// CloseChannel tears down the channel locally. If the channel had actually
// been opened (a DATA frame was sent) it also emits a CLOSE upstream.
func (b *DeviceBridge) CloseChannel(cid uint32) error {
	b.chMu.Lock()
	first, known := b.channelFirstSend[cid]
	delete(b.channelFirstSend, cid)
	delete(b.dataHandlers, cid)
	delete(b.closeHandlers, cid)
	b.chMu.Unlock()

	// Only send CLOSE if we ever actually opened the channel: known &&
	// !first means OPEN went out and we'd been sending DATA since.
	if !known || first {
		return nil
	}
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	if b.ws == nil {
		return nil
	}
	// WS may already be torn down — that's not fatal here.
	_ = b.ws.WriteMessage(websocket.BinaryMessage, encodeFrame(opClose, cid, nil))
	return nil
}

// ResetForReconnect drops every channel from the dead connection. Each
// closeHandler fires as if a CLOSE arrived — passthrough handlers close
// their local usbmuxd connection, so tools see a device blip and retry.
// Channel ids are never reused: nextChannel keeps incrementing.
func (b *DeviceBridge) ResetForReconnect() {
	b.chMu.Lock()
	handlers := make([]closeHandler, 0, len(b.closeHandlers))
	for _, h := range b.closeHandlers {
		handlers = append(handlers, h)
	}
	b.channelFirstSend = map[uint32]bool{}
	b.dataHandlers = map[uint32]dataHandler{}
	b.closeHandlers = map[uint32]closeHandler{}
	b.chMu.Unlock()

	for _, h := range handlers {
		_ = h()
	}
}

// armKeepalive installs the ping/pong machinery on one connection: a read
// deadline that counts down while the link is silent, a pong handler that
// pushes it out (our pings answered), and a ping handler that pushes it
// out and replies with a pong (answering the peer's keepalive).
func (b *DeviceBridge) armKeepalive(ws *websocket.Conn) {
	_ = ws.SetReadDeadline(time.Now().Add(b.pongTimeout))
	ws.SetPongHandler(func(string) error {
		return ws.SetReadDeadline(time.Now().Add(b.pongTimeout))
	})
	ws.SetPingHandler(func(appData string) error {
		_ = ws.SetReadDeadline(time.Now().Add(b.pongTimeout))
		return ws.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(b.writeWait))
	})
}

// pingLoop sends a ping every pingInterval until the context is cancelled
// or a write fails (the reader will surface the actual error).
func (b *DeviceBridge) pingLoop(ctx context.Context, ws *websocket.Conn) {
	ticker := time.NewTicker(b.pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(b.writeWait)); err != nil {
				return
			}
		}
	}
}

// RunUntilError services the current connection: it arms the keepalive,
// starts the ping loop, and reads frames until the connection dies. The
// returned error is the read failure that ended the connection.
func (b *DeviceBridge) RunUntilError(ctx context.Context) error {
	ws := b.currentWS()
	if ws == nil {
		return errors.New("websocket not connected")
	}
	b.armKeepalive(ws)

	pingCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go b.pingLoop(pingCtx, ws)

	return b.readFrames(ws)
}

// readFrames reads frames off one WebSocket connection and dispatches
// them to the per-channel handlers. Returns when the connection dies.
func (b *DeviceBridge) readFrames(ws *websocket.Conn) error {
	for {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		// Any successfully received frame proves the link is alive.
		_ = ws.SetReadDeadline(time.Now().Add(b.pongTimeout))
		if mt != websocket.BinaryMessage {
			log.Printf("ignoring non-binary frame")
			continue
		}
		opcode, cid, payload, err := decodeFrame(data)
		if err != nil {
			log.Printf("bad frame: %v", err)
			continue
		}
		switch opcode {
		case opData:
			b.chMu.Lock()
			handler := b.dataHandlers[cid]
			b.chMu.Unlock()
			if handler == nil {
				continue
			}
			if err := handler(payload); err != nil {
				log.Printf("channel %d handler error: %v", cid, err)
			}
		case opClose:
			b.chMu.Lock()
			handler := b.closeHandlers[cid]
			delete(b.dataHandlers, cid)
			delete(b.closeHandlers, cid)
			delete(b.channelFirstSend, cid)
			b.chMu.Unlock()
			if handler != nil {
				_ = handler()
			}
		default:
			log.Printf("unexpected opcode %d on channel %d", opcode, cid)
		}
	}
}

// DeviceProperties returns a snapshot of the cached device properties.
// Used by the local usbmux handler to answer ListDevices/Listen locally
// without round-tripping for every request.
func (b *DeviceBridge) DeviceProperties() map[string]interface{} {
	b.propsMu.Lock()
	defer b.propsMu.Unlock()
	if b.deviceProperties == nil {
		return nil
	}
	out := make(map[string]interface{}, len(b.deviceProperties))
	for k, v := range b.deviceProperties {
		out[k] = v
	}
	return out
}

// LocalDeviceID is the per-session, client-side device id we expose to
// the local libimobiledevice stack. The Sauce side only has one device
// per session, so this is always 1.
func (b *DeviceBridge) LocalDeviceID() int {
	return b.localDeviceID
}

// FetchDeviceProperties opens a transient multiplex channel, sends one
// ListDevices PLIST, parses the response, and caches the resulting
// Properties dict on the bridge. The reader loop must be running. Also
// doubles as the post-(re)connect health check: a reply proves the tunnel
// is up end to end.
func (b *DeviceBridge) FetchDeviceProperties(ctx context.Context, timeout time.Duration) (map[string]interface{}, error) {
	cid := b.AllocChannel()
	defer func() { _ = b.CloseChannel(cid) }()

	resultCh := make(chan map[string]interface{}, 1)
	errCh := make(chan error, 1)
	var buf []byte

	onData := func(data []byte) error {
		buf = append(buf, data...)
		msg, _, ok, err := tryDecodeUsbmuxMessage(buf)
		if err != nil {
			select {
			case errCh <- err:
			default:
			}
			return nil
		}
		if !ok {
			return nil
		}
		deviceList, _ := msg.Payload["DeviceList"].([]interface{})
		if len(deviceList) == 0 {
			select {
			case errCh <- errors.New("DeviceList empty in server response"):
			default:
			}
			return nil
		}
		device, _ := deviceList[0].(map[string]interface{})
		properties, _ := device["Properties"].(map[string]interface{})
		if properties == nil {
			select {
			case errCh <- errors.New("missing Properties in DeviceList[0]"):
			default:
			}
			return nil
		}
		select {
		case resultCh <- properties:
		default:
		}
		return nil
	}
	onClose := func() error {
		select {
		case errCh <- errors.New("server closed channel before responding"):
		default:
		}
		return nil
	}
	b.RegisterHandlers(cid, onData, onClose)

	req, err := encodeUsbmuxMessage(usbmuxVersionPlist, usbmuxTypePlist, 1, map[string]interface{}{
		"MessageType": "ListDevices",
	})
	if err != nil {
		return nil, err
	}
	if err := b.SendToChannel(cid, req); err != nil {
		return nil, err
	}

	select {
	case props := <-resultCh:
		b.propsMu.Lock()
		b.deviceProperties = props
		b.propsMu.Unlock()
		return props, nil
	case err := <-errCh:
		return nil, err
	case <-time.After(timeout):
		return nil, errors.New("timeout waiting for device properties")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
