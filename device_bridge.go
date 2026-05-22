package main

import (
	"context"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// dataHandler is invoked when the server sends a DATA frame on a channel
// the local side has registered for. closeHandler fires when the server
// sends a CLOSE on that channel.
type dataHandler func([]byte) error
type closeHandler func() error

// DeviceBridge owns one multiplexed WebSocket and the per-channel routing
// tables for the iOS path.
//
// The first send on a channel uses OPEN; every subsequent send uses DATA;
// an explicit CLOSE goes out when the local end of the channel goes away.
type DeviceBridge struct {
	wsURL     string
	sessionID string
	authB64   string

	ws     *websocket.Conn
	sendMu sync.Mutex // serializes WriteMessage

	chMu             sync.Mutex
	nextChannel      uint32
	channelFirstSend map[uint32]bool
	dataHandlers     map[uint32]dataHandler
	closeHandlers    map[uint32]closeHandler

	propsMu          sync.Mutex
	deviceProperties map[string]interface{}
	localDeviceID    int
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
	}
}

// Connect dials the WebSocket using the supplied sessionId / basic-auth
// headers. The connection then services every channel until Close.
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
	b.ws = ws
	return nil
}

// Close drops the underlying WebSocket. Safe to call multiple times.
func (b *DeviceBridge) Close() error {
	if b.ws == nil {
		return nil
	}
	return b.ws.Close()
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
// DATA on subsequent calls).
func (b *DeviceBridge) SendToChannel(cid uint32, payload []byte) error {
	b.chMu.Lock()
	first, known := b.channelFirstSend[cid]
	var opcode uint16 = opData
	if !known || first {
		opcode = opOpen
	}
	b.channelFirstSend[cid] = false
	b.chMu.Unlock()

	b.sendMu.Lock()
	defer b.sendMu.Unlock()
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
	if !known || first || b.ws == nil {
		return nil
	}
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	// WS may already be torn down — that's not fatal here.
	_ = b.ws.WriteMessage(websocket.BinaryMessage, encodeFrame(opClose, cid, nil))
	return nil
}

// ReaderLoop reads frames off the WebSocket and dispatches them to the
// per-channel handlers. Returns when the WebSocket closes.
func (b *DeviceBridge) ReaderLoop(ctx context.Context) error {
	for {
		mt, data, err := b.ws.ReadMessage()
		if err != nil {
			return err
		}
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
// Properties dict on the bridge.
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
