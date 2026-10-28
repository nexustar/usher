package codex

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// echoWS upgrades connections and echoes text frames, so the test covers the
// handshake and framing without knowing any app-server method.
func echoWS(t *testing.T, network, addr, token string) string {
	t.Helper()
	ln, err := net.Listen(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				req, err := http.ReadRequest(r)
				if err != nil {
					return
				}
				if token != "" && req.Header.Get("Authorization") != "Bearer "+token {
					fmt.Fprint(conn, "HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\n")
					return
				}
				fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
					wsAccept(req.Header.Get("Sec-WebSocket-Key")))
				for {
					_, op, payload, err := readFrame(r, maxMessage)
					if err != nil {
						return
					}
					switch op {
					case opText:
						_ = writeFrame(conn, opText, payload, false)
					case opPing:
						_ = writeFrame(conn, opPong, payload, false)
					case opClose:
						_ = writeFrame(conn, opClose, payload, false)
						return
					}
				}
			}()
		}
	}()
	if network == "unix" {
		return "unix://" + addr
	}
	return "ws://" + ln.Addr().String()
}

func TestFrameRoundTrip(t *testing.T) {
	for _, n := range []int{0, 125, 126, 65535, 65536} {
		for _, mask := range []bool{false, true} {
			payload := bytes.Repeat([]byte{byte(n)}, n)
			var buf bytes.Buffer
			if err := writeFrame(&buf, opText, payload, mask); err != nil {
				t.Fatal(err)
			}
			fin, op, got, err := readFrame(bufio.NewReader(&buf), maxMessage)
			if err != nil || !fin || op != opText || !bytes.Equal(got, payload) {
				t.Fatalf("n=%d mask=%v: fin=%v op=%d len=%d err=%v", n, mask, fin, op, len(got), err)
			}
		}
	}
	var buf bytes.Buffer
	_ = writeFrame(&buf, opText, make([]byte, 10), true)
	if _, _, _, err := readFrame(bufio.NewReader(&buf), 5); err == nil {
		t.Fatal("oversized frame was accepted")
	}
}

func TestDialLink(t *testing.T) {
	// macOS caps socket paths at 104 bytes, below t.TempDir.
	dir, err := os.MkdirTemp("/tmp", "usher-ws")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, url := range []string{
		echoWS(t, "tcp", "127.0.0.1:0", "secret"),
		echoWS(t, "unix", filepath.Join(dir, "s.sock"), "secret"),
	} {
		if _, err := dialLink(ctx, Attach{URL: url}); err == nil || !strings.Contains(err.Error(), "401") {
			t.Fatalf("%s without token: %v", url, err)
		}
		l, err := dialLink(ctx, Attach{URL: url, TokenFile: tokenFile})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := l.Write([]byte(`{"id":1}` + "\n")); err != nil {
			t.Fatal(err)
		}
		if b, err := l.Read(); err != nil || string(b) != `{"id":1}` {
			t.Fatalf("read %q, %v", b, err)
		}
		if err := l.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := l.Read(); err != io.EOF {
			t.Fatalf("read after close: %v", err)
		}
		<-l.Done()
	}
	for _, bad := range []string{"unix://relative/x.sock", "unix://", "wss://x", "http://x", "ws://"} {
		if _, err := dialLink(ctx, Attach{URL: bad}); err == nil {
			t.Errorf("dial %q succeeded", bad)
		}
	}
}
