package bridge

import (
	"encoding/binary"
	"fmt"

	"howett.net/plist"
)

// usbmuxd PLIST framing — the wire protocol Apple's usbmuxd speaks.
//
//	| 4-byte length (LE, incl. header) | 4-byte version | 4-byte type | 4-byte tag | XML PLIST body |
//
// Version 1 + type 8 together mean "PLIST message"; tag is a request id
// the sender uses to correlate responses.

const (
	usbmuxdHeadLen     = 16
	usbmuxVersionPlist = 1
	usbmuxTypePlist    = 8
)

type usbmuxMessage struct {
	Version uint32
	Type    uint32
	Tag     uint32
	Payload map[string]interface{}
}

// encodeUsbmuxMessage serializes a PLIST message ready to be sent on the
// wire (or wrapped in a multiplex frame).
func encodeUsbmuxMessage(version, type_, tag uint32, payload interface{}) ([]byte, error) {
	body, err := plist.Marshal(payload, plist.XMLFormat)
	if err != nil {
		return nil, fmt.Errorf("plist marshal: %w", err)
	}
	length := usbmuxdHeadLen + len(body)
	buf := make([]byte, length)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(length))
	binary.LittleEndian.PutUint32(buf[4:8], version)
	binary.LittleEndian.PutUint32(buf[8:12], type_)
	binary.LittleEndian.PutUint32(buf[12:16], tag)
	copy(buf[usbmuxdHeadLen:], body)
	return buf, nil
}

// tryDecodeUsbmuxMessage pulls one full usbmux message off the front of
// buf. Partial-read safe: returns (nil, buf, false, nil) when the buffer
// doesn't yet hold a complete message; the caller is expected to append
// more bytes and re-call.
func tryDecodeUsbmuxMessage(buf []byte) (msg *usbmuxMessage, rest []byte, ok bool, err error) {
	if len(buf) < usbmuxdHeadLen {
		return nil, buf, false, nil
	}
	length := binary.LittleEndian.Uint32(buf[0:4])
	if length < usbmuxdHeadLen || uint32(len(buf)) < length {
		return nil, buf, false, nil
	}
	version := binary.LittleEndian.Uint32(buf[4:8])
	type_ := binary.LittleEndian.Uint32(buf[8:12])
	tag := binary.LittleEndian.Uint32(buf[12:16])
	body := buf[usbmuxdHeadLen:length]

	payload := map[string]interface{}{}
	if len(body) > 0 {
		if _, err := plist.Unmarshal(body, &payload); err != nil {
			return nil, buf, false, fmt.Errorf("plist unmarshal: %w", err)
		}
	}
	return &usbmuxMessage{
		Version: version,
		Type:    type_,
		Tag:     tag,
		Payload: payload,
	}, buf[length:], true, nil
}
