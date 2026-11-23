package webui

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/crossparties/tether/internal/core"
)

type testWSClient struct {
	conn net.Conn
	br   *bufio.Reader
}

func dialTestWS(t *testing.T, addr, path string, cookie *http.Cookie, origin string) (*testWSClient, int, error) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nil, 0, err
	}
	var keyRaw [16]byte
	_, _ = rand.Read(keyRaw[:])
	wsKey := base64.StdEncoding.EncodeToString(keyRaw[:])

	var req strings.Builder
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&req, "Host: %s\r\n", addr)
	req.WriteString("Upgrade: websocket\r\n")
	req.WriteString("Connection: Upgrade\r\n")
	req.WriteString("Sec-WebSocket-Version: 13\r\n")
	fmt.Fprintf(&req, "Sec-WebSocket-Key: %s\r\n", wsKey)
	if origin != "" {
		fmt.Fprintf(&req, "Origin: %s\r\n", origin)
	}
	if cookie != nil {
		fmt.Fprintf(&req, "Cookie: %s=%s\r\n", cookie.Name, cookie.Value)
	}
	req.WriteString("\r\n")

	if _, err := conn.Write([]byte(req.String())); err != nil {
		_ = conn.Close()
		return nil, 0, err
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		_ = conn.Close()
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = resp.Body.Close()
		_ = conn.Close()
		return nil, resp.StatusCode, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	sum := sha1.Sum([]byte(wsKey + wsGUID))
	wantAccept := base64.StdEncoding.EncodeToString(sum[:])
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != wantAccept {
		_ = conn.Close()
		return nil, resp.StatusCode, fmt.Errorf("bad Sec-WebSocket-Accept: got %q want %q", got, wantAccept)
	}
	return &testWSClient{conn: conn, br: br}, resp.StatusCode, nil
}

func (c *testWSClient) writeMaskedFrame(opcode byte, payload []byte) error {
	var hdr [14]byte
	hdr[0] = 0x80 | (opcode & 0x0F)
	n := len(payload)
	idx := 2
	switch {
	case n <= 125:
		hdr[1] = 0x80 | byte(n)
	case n <= 65535:
		hdr[1] = 0x80 | 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
		idx = 4
	default:
		hdr[1] = 0x80 | 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(n))
		idx = 10
	}
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	copy(hdr[idx:idx+4], mask[:])
	idx += 4

	buf := make([]byte, idx+n)
	copy(buf[:idx], hdr[:idx])
	for i := range payload {
		buf[idx+i] = payload[i] ^ mask[i%4]
	}
	_, err := c.conn.Write(buf)
	return err
}

func (c *testWSClient) readServerFrame() (byte, []byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		return 0, nil, err
	}
	opcode := hdr[0] & 0x0F
	length := uint64(hdr[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	payload := make([]byte, int(length))
	if length > 0 {
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return 0, nil, err
		}
	}
	return opcode, payload, nil
}

func (c *testWSClient) close() {
	_ = c.writeMaskedFrame(wsOpClose, []byte{0x03, 0xe8})
	_ = c.conn.Close()
}

func TestWebSocketPTYRoundtripResizeAndEnvSanitization(t *testing.T) {
	t.Setenv("TETHER_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TETHER_API_KEY", "secret_tether_val")
	t.Setenv("DEEPSEEK_API_KEY", "secret_ds_val")

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, ".env"), []byte("DOTENV_SECRET=secret_dotenv_val\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	eng, err := core.NewEngine(ws)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	srv, err := NewServer(Config{Engine: eng, Port: 0})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	addr, err := srv.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		<-serveErr
	}()

	// 1. Unauthenticated WS rejected 401.
	if _, status, derr := dialTestWS(t, addr, "/api/v1/terminal/ws", nil, "http://"+addr); derr == nil || status != http.StatusUnauthorized {
		t.Fatalf("expected 401 on unauthenticated WS, got status=%d err=%v", status, derr)
	}

	// Redeem bootstrap cookie.
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	bootResp, err := hc.Get("http://" + addr + "/?token=" + srv.BootstrapToken())
	if err != nil {
		t.Fatalf("bootstrap GET: %v", err)
	}
	bootResp.Body.Close()
	var sessCookie *http.Cookie
	for _, ck := range bootResp.Cookies() {
		if ck.Name == sessionCookieName {
			sessCookie = ck
		}
	}
	if sessCookie == nil {
		t.Fatal("missing session cookie")
	}

	// 2. Wrong Origin WS rejected 403.
	if _, status, derr := dialTestWS(t, addr, "/api/v1/terminal/ws", sessCookie, "https://evil.example"); derr == nil || status != http.StatusForbidden {
		t.Fatalf("expected 403 on evil Origin WS, got status=%d err=%v", status, derr)
	}

	if runtime.GOOS != "linux" {
		t.Skip("Linux PTY roundtrip skipped on non-Linux OS")
	}

	// 3. Valid WS + PTY roundtrip, resize, and secret env check.
	wsc, status, err := dialTestWS(t, addr, "/api/v1/terminal/ws", sessCookie, "http://"+addr)
	if err != nil {
		t.Fatalf("dialTestWS failed (status=%d): %v", status, err)
	}

	outCh := make(chan string, 64)
	go func() {
		for {
			op, data, rerr := wsc.readServerFrame()
			if rerr != nil {
				return
			}
			if op == wsOpPing {
				_ = wsc.writeMaskedFrame(wsOpPong, data)
				continue
			}
			if op == wsOpClose {
				return
			}
			if (op == wsOpText || op == wsOpBinary) && len(data) > 0 {
				outCh <- string(data)
			}
		}
	}()

	waitForOutput := func(substr string) string {
		t.Helper()
		var acc strings.Builder
		deadline := time.After(4 * time.Second)
		for {
			if strings.Contains(acc.String(), substr) {
				return acc.String()
			}
			select {
			case chunk := <-outCh:
				acc.WriteString(chunk)
			case <-deadline:
				t.Fatalf("timed out waiting for %q in PTY output:\n%s", substr, acc.String())
			}
		}
	}

	if err := wsc.writeMaskedFrame(wsOpText, []byte("echo hello_pty\n")); err != nil {
		t.Fatalf("write echo: %v", err)
	}
	waitForOutput("hello_pty")

	if err := wsc.writeMaskedFrame(wsOpText, []byte(`{"type":"resize","cols":120,"rows":40}`)); err != nil {
		t.Fatalf("write resize: %v", err)
	}
	if err := wsc.writeMaskedFrame(wsOpText, []byte("stty size\n")); err != nil {
		t.Fatalf("write stty size: %v", err)
	}
	waitForOutput("40 120")

	if err := wsc.writeMaskedFrame(wsOpText, []byte("env && echo ENV_CHECK_DONE\n")); err != nil {
		t.Fatalf("write env check: %v", err)
	}
	envOut := waitForOutput("ENV_CHECK_DONE")
	for _, secret := range []string{"secret_tether_val", "secret_ds_val", "secret_dotenv_val", "TETHER_API_KEY", "DEEPSEEK_API_KEY"} {
		if strings.Contains(envOut, secret) {
			t.Fatalf("PTY environment leaked secret %q!\n%s", secret, envOut)
		}
	}

	wsc.close()
}

func readCloseStatus(t *testing.T, wsc *testWSClient) uint16 {
	t.Helper()
	_ = wsc.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		op, payload, err := wsc.readServerFrame()
		if err != nil {
			t.Fatalf("expected close frame before read error: %v", err)
		}
		if op == wsOpClose {
			if len(payload) < 2 {
				t.Fatalf("close frame missing status code: %x", payload)
			}
			return binary.BigEndian.Uint16(payload[:2])
		}
	}
}

func TestWebSocketProtocolEdgeCasesAndBounds(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux PTY WebSocket edge tests skipped on non-Linux OS")
	}
	t.Setenv("TETHER_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	ws := t.TempDir()
	eng, err := core.NewEngine(ws)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	srv, err := NewServer(Config{Engine: eng, Port: 0})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	addr, err := srv.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve() }()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		<-errCh
	}()

	noRedirectClient := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := noRedirectClient.Get("http://" + addr + "/?token=" + srv.BootstrapToken())
	if err != nil {
		t.Fatalf("redeem bootstrap: %v", err)
	}
	var sessCookie *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == sessionCookieName {
			sessCookie = ck
		}
	}
	resp.Body.Close()
	if sessCookie == nil {
		t.Fatal("missing session cookie")
	}

	dial := func() *testWSClient {
		wsc, status, derr := dialTestWS(t, addr, "/api/v1/terminal/ws", sessCookie, "http://"+addr)
		if derr != nil {
			t.Fatalf("dialTestWS failed (status=%d): %v", status, derr)
		}
		return wsc
	}

	// 1. Unmasked client frame -> close 1002.
	wscUnmasked := dial()
	_, _ = wscUnmasked.conn.Write([]byte{0x81, 0x05, 'h', 'e', 'l', 'l', 'o'})
	if code := readCloseStatus(t, wscUnmasked); code != 1002 {
		t.Fatalf("expected 1002 on unmasked frame, got %d", code)
	}
	_ = wscUnmasked.conn.Close()

	// 2. Oversized >1MB frame -> close 1009.
	wscBig := dial()
	var bigHdr [14]byte
	bigHdr[0] = 0x82 // FIN + binary
	bigHdr[1] = 0x80 | 127
	binary.BigEndian.PutUint64(bigHdr[2:10], maxWSFrameBytes+1)
	_, _ = wscBig.conn.Write(bigHdr[:])
	if code := readCloseStatus(t, wscBig); code != 1009 {
		t.Fatalf("expected 1009 on >1MB frame, got %d", code)
	}
	_ = wscBig.conn.Close()

	// 3. Unexpected continuation opcode without initial fragment -> close 1002.
	wscCont := dial()
	if err := wscCont.writeMaskedFrame(wsOpContinuation, []byte("orphan")); err != nil {
		t.Fatalf("write continuation frame: %v", err)
	}
	if code := readCloseStatus(t, wscCont); code != 1002 {
		t.Fatalf("expected 1002 on unexpected continuation frame, got %d", code)
	}
	_ = wscCont.conn.Close()

	// 4. Out-of-bounds resize: cols<=0 ignored without writing JSON into PTY stdin; cols>500 clamped to 500.
	wscBounds := dial()
	defer wscBounds.close()

	outCh := make(chan string, 64)
	go func() {
		for {
			op, data, rerr := wscBounds.readServerFrame()
			if rerr != nil {
				return
			}
			if (op == wsOpText || op == wsOpBinary) && len(data) > 0 {
				outCh <- string(data)
			}
		}
	}()

	_ = wscBounds.writeMaskedFrame(wsOpText, []byte(`{"type":"resize","cols":0,"rows":24}`))
	_ = wscBounds.writeMaskedFrame(wsOpText, []byte(`{"type":"resize","cols":9999,"rows":600}`))
	_ = wscBounds.writeMaskedFrame(wsOpText, []byte("stty size && echo __RESIZE\"_\"DONE__\n"))

	var acc strings.Builder
	deadline := time.After(4 * time.Second)
	for !strings.Contains(acc.String(), "__RESIZE_DONE__") {
		select {
		case chunk := <-outCh:
			acc.WriteString(chunk)
		case <-deadline:
			t.Fatalf("timed out waiting for stty size:\n%s", acc.String())
		}
	}
	out := acc.String()
	if strings.Contains(out, `"type":"resize"`) {
		t.Fatalf("invalid resize JSON leaked into shell stdin:\n%s", out)
	}
	if !strings.Contains(out, "500 500") {
		t.Fatalf("expected clamped PTY dimensions '500 500', got:\n%s", out)
	}
}
