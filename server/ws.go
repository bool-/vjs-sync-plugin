package main

// A minimal RFC 6455 WebSocket server: handshake, masked client frames,
// fragmentation, ping/pong and close. Enough for small JSON text messages;
// no extensions, no compression.

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA

	wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

var errTooLarge = errors.New("websocket: message too large")
var errProtocol = errors.New("websocket: protocol error")

type wsConn struct {
	conn       net.Conn
	r          *bufio.Reader
	writeMu    sync.Mutex
	maxMessage int
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// sameOrigin rejects cross-site pages opening a socket with the viewer's
// cookies. Non-browser clients send no Origin and are let through.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func acceptKey(key string) string {
	h := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

func upgrade(w http.ResponseWriter, r *http.Request, maxMessage int) (*wsConn, error) {
	key := r.Header.Get("Sec-Websocket-Key")
	if r.Method != http.MethodGet ||
		!headerHasToken(r.Header, "Connection", "upgrade") ||
		!headerHasToken(r.Header, "Upgrade", "websocket") ||
		r.Header.Get("Sec-Websocket-Version") != "13" || key == "" {
		http.Error(w, "websocket upgrade required", http.StatusBadRequest)
		return nil, errProtocol
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin websocket refused", http.StatusForbidden)
		return nil, errProtocol
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket unsupported", http.StatusInternalServerError)
		return nil, errProtocol
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	_, err = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey(key) + "\r\n\r\n")
	if err == nil {
		err = rw.Flush()
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &wsConn{conn: conn, r: rw.Reader, maxMessage: maxMessage}, nil
}

func (c *wsConn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var head [2]byte
	if _, err = io.ReadFull(c.r, head[:]); err != nil {
		return
	}
	fin = head[0]&0x80 != 0
	op = head[0] & 0x0F
	if head[0]&0x70 != 0 || head[1]&0x80 == 0 {
		// Reserved bits need an extension we never agreed to; client frames
		// must be masked.
		err = errProtocol
		return
	}
	length := uint64(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.r, ext[:]); err != nil {
			return
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.r, ext[:]); err != nil {
			return
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if op >= opClose && (length > 125 || !fin) {
		err = errProtocol
		return
	}
	if length > uint64(c.maxMessage) {
		err = errTooLarge
		return
	}
	var mask [4]byte
	if _, err = io.ReadFull(c.r, mask[:]); err != nil {
		return
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(c.r, payload); err != nil {
		return
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return
}

// ReadMessage returns the next text or binary message, answering pings and
// close frames along the way. It returns io.EOF once the peer closes.
func (c *wsConn) ReadMessage(idle time.Duration) ([]byte, error) {
	var message []byte
	started := false
	for {
		c.conn.SetReadDeadline(time.Now().Add(idle))
		fin, op, payload, err := c.readFrame()
		if err != nil {
			if errors.Is(err, errTooLarge) {
				c.closeWith(1009)
			} else if errors.Is(err, errProtocol) {
				c.closeWith(1002)
			}
			return nil, err
		}
		switch op {
		case opPing:
			if err := c.writeFrame(opPong, payload); err != nil {
				return nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			c.writeFrame(opClose, payload)
			return nil, io.EOF
		case opText, opBinary:
			if started {
				c.closeWith(1002)
				return nil, errProtocol
			}
			started = true
			message = payload
		case opContinuation:
			if !started {
				c.closeWith(1002)
				return nil, errProtocol
			}
			if len(message)+len(payload) > c.maxMessage {
				c.closeWith(1009)
				return nil, errTooLarge
			}
			message = append(message, payload...)
		default:
			c.closeWith(1002)
			return nil, errProtocol
		}
		if fin {
			return message, nil
		}
	}
}

func (c *wsConn) writeFrame(op byte, payload []byte) error {
	header := make([]byte, 2, 10)
	header[0] = 0x80 | op
	switch n := len(payload); {
	case n < 126:
		header[1] = byte(n)
	case n <= 0xFFFF:
		header[1] = 126
		header = binary.BigEndian.AppendUint16(header, uint16(n))
	default:
		header[1] = 127
		header = binary.BigEndian.AppendUint64(header, uint64(n))
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := (&net.Buffers{header, payload}).WriteTo(c.conn)
	return err
}

func (c *wsConn) WriteText(payload []byte) error {
	return c.writeFrame(opText, payload)
}

func (c *wsConn) closeWith(code uint16) {
	c.writeFrame(opClose, binary.BigEndian.AppendUint16(nil, code))
}

func (c *wsConn) Close() error {
	return c.conn.Close()
}
