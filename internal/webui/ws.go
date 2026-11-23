package webui

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	wsGUID          = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	maxWSFrameBytes = 1 << 20 // 1MB payload cap

	wsOpContinuation byte = 0x0
	wsOpText         byte = 0x1
	wsOpBinary       byte = 0x2
	wsOpClose        byte = 0x8
	wsOpPing         byte = 0x9
	wsOpPong         byte = 0xA
)

type ptySession interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Resize(cols, rows int) error
	Close() error
}

// sanitizePTYEnv builds a strict allowlist environment for the interactive web
// terminal so secrets from host env, .env, or credentials.json never leak.
func sanitizePTYEnv(workspace string) []string {
	allowed := map[string]bool{
		"PATH":     true,
		"HOME":     true,
		"USER":     true,
		"LOGNAME":  true,
		"SHELL":    true,
		"LANG":     true,
		"LC_ALL":   true,
		"LC_CTYPE": true,
		"TMPDIR":   true,
		"TZ":       true,
		"EDITOR":   true,
		"VISUAL":   true,
	}
	var env []string
	for _, entry := range os.Environ() {
		k, _, ok := strings.Cut(entry, "=")
		if !ok || !allowed[k] {
			continue
		}
		env = append(env, entry)
	}
	env = append(env,
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"PWD="+workspace,
	)
	return env
}

type wsConn struct {
	conn    net.Conn
	br      *bufio.Reader
	writeMu sync.Mutex
	closed  bool
}

func acceptWebSocket(w http.ResponseWriter, r *http.Request) (*wsConn, bool, error) {
	if r.Method != http.MethodGet {
		return nil, false, fmt.Errorf("websocket requires GET")
	}
	if !headerHasToken(r.Header.Get("Connection"), "upgrade") ||
		!strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return nil, false, fmt.Errorf("missing Upgrade: websocket header")
	}
	if strings.TrimSpace(r.Header.Get("Sec-WebSocket-Version")) != "13" {
		return nil, false, fmt.Errorf("unsupported Sec-WebSocket-Version")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		return nil, false, fmt.Errorf("missing Sec-WebSocket-Key")
	}

	sum := sha1.Sum([]byte(key + wsGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])

	rc := http.NewResponseController(w)
	conn, rw, err := rc.Hijack()
	if err != nil {
		return nil, false, fmt.Errorf("hijack connection: %w", err)
	}

	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := rw.WriteString(resp); err != nil {
		_ = conn.Close()
		return nil, true, err
	}
	if err := rw.Flush(); err != nil {
		_ = conn.Close()
		return nil, true, err
	}

	return &wsConn{
		conn: conn,
		br:   rw.Reader,
	}, true, nil
}

func headerHasToken(headerVal, want string) bool {
	for part := range strings.SplitSeq(headerVal, ",") {
		if strings.EqualFold(strings.TrimSpace(part), want) {
			return true
		}
	}
	return false
}

func (c *wsConn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		return false, 0, nil, err
	}
	fin = (hdr[0] & 0x80) != 0
	if (hdr[0] & 0x70) != 0 {
		return false, 0, nil, fmt.Errorf("unsupported RSV bits")
	}
	opcode = hdr[0] & 0x0F
	masked := (hdr[1] & 0x80) != 0
	if !masked {
		return false, 0, nil, fmt.Errorf("unmasked client frame")
	}

	length := uint64(hdr[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		if (ext[0] & 0x80) != 0 {
			return false, 0, nil, fmt.Errorf("invalid 64-bit frame length")
		}
		length = binary.BigEndian.Uint64(ext[:])
	}

	if opcode >= 0x8 {
		if !fin || length > 125 {
			return false, 0, nil, fmt.Errorf("invalid control frame")
		}
	}
	if length > maxWSFrameBytes {
		return false, 0, nil, errWSMessageTooBig
	}

	var mask [4]byte
	if _, err := io.ReadFull(c.br, mask[:]); err != nil {
		return false, 0, nil, err
	}
	payload = make([]byte, int(length))
	if length > 0 {
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return false, 0, nil, err
		}
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return fin, opcode, payload, nil
}

var errWSMessageTooBig = errors.New("websocket message exceeds 1MB limit")

// ReadMessage reads a complete (possibly fragmented) client message while answering
// interleaved Ping frames and handling Close frames automatically.
func (c *wsConn) ReadMessage() (byte, []byte, error) {
	var msgOpcode byte
	var msgBuf []byte

	for {
		fin, opcode, payload, err := c.readFrame()
		if err != nil {
			if errors.Is(err, errWSMessageTooBig) {
				_ = c.Close(1009, "message too big")
			} else {
				_ = c.Close(1002, err.Error())
			}
			return 0, nil, err
		}

		switch opcode {
		case wsOpPing:
			_ = c.WriteFrame(wsOpPong, payload)
			continue
		case wsOpPong:
			continue
		case wsOpClose:
			_ = c.Close(1000, "closed")
			return wsOpClose, payload, io.EOF
		case wsOpText, wsOpBinary:
			if msgOpcode != 0 {
				_ = c.Close(1002, "unexpected data frame")
				return 0, nil, fmt.Errorf("interleaved data frame")
			}
			if fin {
				return opcode, payload, nil
			}
			msgOpcode = opcode
			msgBuf = append(msgBuf, payload...)
		case wsOpContinuation:
			if msgOpcode == 0 {
				_ = c.Close(1002, "unexpected continuation frame")
				return 0, nil, fmt.Errorf("unexpected continuation frame")
			}
			if len(msgBuf)+len(payload) > maxWSFrameBytes {
				_ = c.Close(1009, "message too big")
				return 0, nil, errWSMessageTooBig
			}
			msgBuf = append(msgBuf, payload...)
			if fin {
				return msgOpcode, msgBuf, nil
			}
		default:
			_ = c.Close(1002, "unknown opcode")
			return 0, nil, fmt.Errorf("unknown opcode 0x%x", opcode)
		}
	}
}

// WriteFrame writes an unmasked server-to-client RFC 6455 frame safely under writeMu.
func (c *wsConn) WriteFrame(opcode byte, payload []byte) error {
	if len(payload) > maxWSFrameBytes {
		return errWSMessageTooBig
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed {
		return net.ErrClosed
	}

	var hdr [10]byte
	hdr[0] = 0x80 | (opcode & 0x0F)
	n := len(payload)
	hdrLen := 2
	switch {
	case n <= 125:
		hdr[1] = byte(n)
	case n <= 65535:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
		hdrLen = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(n))
		hdrLen = 10
	}
	if _, err := c.conn.Write(hdr[:hdrLen]); err != nil {
		return err
	}
	if n > 0 {
		if _, err := c.conn.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// Close sends a Close frame (once) and closes the underlying TCP connection.
func (c *wsConn) Close(code uint16, reason string) error {
	c.writeMu.Lock()
	if c.closed {
		c.writeMu.Unlock()
		return nil
	}
	var body []byte
	if code > 0 {
		reasonBytes := []byte(reason)
		if len(reasonBytes) > 123 {
			reasonBytes = reasonBytes[:123]
		}
		body = make([]byte, 2+len(reasonBytes))
		binary.BigEndian.PutUint16(body[:2], code)
		copy(body[2:], reasonBytes)
	}
	var hdr [4]byte
	hdr[0] = 0x80 | wsOpClose
	hdr[1] = byte(len(body))
	_, _ = c.conn.Write(hdr[:2])
	if len(body) > 0 {
		_, _ = c.conn.Write(body)
	}
	c.closed = true
	c.writeMu.Unlock()
	return c.conn.Close()
}

func (s *Server) handleTerminalWS(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAuth(w, r, false); !ok {
		return
	}

	ws, hijacked, err := acceptWebSocket(w, r)
	if err != nil {
		if !hijacked {
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}

	eng, _ := s.activeState()
	workspace := "."
	if eng != nil && eng.Workspace != "" {
		workspace = eng.Workspace
	}

	pty, err := startPTY(workspace, 80, 24)
	if err != nil {
		_ = ws.Close(1011, err.Error())
		return
	}

	cleanup := func() {
		_ = pty.Close()
		_ = ws.Close(1000, "terminal closed")
	}
	termID := s.registerTerminalCleanup(cleanup)
	defer func() {
		s.unregisterTerminalCleanup(termID)
		cleanup()
	}()

	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		buf := make([]byte, 8192)
		for {
			n, rerr := pty.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				op := wsOpBinary
				if utf8.Valid(chunk) {
					op = wsOpText
				}
				if werr := ws.WriteFrame(op, chunk); werr != nil {
					return
				}
			}
			if rerr != nil {
				_ = ws.Close(1000, "shell exited")
				return
			}
		}
	}()

	for {
		op, payload, rerr := ws.ReadMessage()
		if rerr != nil {
			break
		}
		if op == wsOpText || op == wsOpBinary {
			if op == wsOpText && handleWSControlMessage(pty, payload) {
				continue
			}
			if len(payload) > 0 {
				if _, werr := pty.Write(payload); werr != nil {
					break
				}
			}
		}
	}

	cleanup()
	<-pumpDone
}

func handleWSControlMessage(pty ptySession, payload []byte) bool {
	trimmed := strings.TrimSpace(string(payload))
	if !strings.HasPrefix(trimmed, "{") {
		return false
	}
	var ctrl struct {
		Type string `json:"type"`
		Cols int    `json:"cols"`
		Rows int    `json:"rows"`
	}
	if err := json.Unmarshal([]byte(trimmed), &ctrl); err != nil {
		return false
	}
	if strings.EqualFold(ctrl.Type, "resize") {
		if ctrl.Cols > 0 && ctrl.Rows > 0 {
			_ = pty.Resize(ctrl.Cols, ctrl.Rows)
		}
		return true
	}
	return false
}
