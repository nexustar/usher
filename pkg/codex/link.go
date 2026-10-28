package codex

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nexustar/usher/pkg/procutil"
)

// link is one connection to an app-server. Write takes one newline-terminated
// JSON-RPC message, Read returns one without the newline. Close is graceful,
// Kill immediate; Done closes once the peer is gone and Err says why.
type link interface {
	io.WriteCloser
	Read() ([]byte, error)
	Kill()
	Done() <-chan struct{}
	Err() error
}

// Attach names an app-server to connect to instead of spawning one:
// `unix://PATH` or `ws://host:port`. TokenFile holds a websocket listener's
// bearer token (--ws-auth), read on every dial.
type Attach struct {
	URL       string
	TokenFile string
}

// DefaultControlSocket is the control socket `codex app-server --listen
// unix://` binds under a CODEX_HOME.
func DefaultControlSocket(codexHome string) string {
	return filepath.Join(codexHome, "app-server-control", "app-server-control.sock")
}

// maxMessage bounds one app-server message on both transports.
const maxMessage = 64 << 20

// stdioLink is a spawned `codex app-server` speaking JSONL on its pipes.
type stdioLink struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Scanner
	done chan struct{}
	err  error
}

func spawnLink(bin string, args, env []string) (*stdioLink, error) {
	cmd := exec.CommandContext(context.Background(), bin, args...)
	procutil.ConfigureGroup(cmd)
	cmd.Env = env
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := bufio.NewScanner(out)
	s.Buffer(make([]byte, 64<<10), maxMessage)
	l := &stdioLink{cmd: cmd, in: in, out: s, done: make(chan struct{})}
	go func() {
		l.err = fmt.Errorf("codex app-server exited: %w", cmd.Wait())
		close(l.done)
	}()
	return l, nil
}

func (l *stdioLink) Write(p []byte) (int, error) { return l.in.Write(p) }

func (l *stdioLink) Read() ([]byte, error) {
	if l.out.Scan() {
		return l.out.Bytes(), nil
	}
	err := l.out.Err()
	if err == nil {
		err = io.EOF
	}
	return nil, fmt.Errorf("app-server stdout closed: %w", err)
}

func (l *stdioLink) Close() error { return l.in.Close() }

func (l *stdioLink) Kill() {
	_ = l.in.Close()
	_ = procutil.KillGroup(l.cmd)
}

func (l *stdioLink) Done() <-chan struct{} { return l.done }

func (l *stdioLink) Err() error {
	select {
	case <-l.done:
		return l.err
	default:
		return nil
	}
}

// wsLink is a websocket connection to a listening app-server, one JSON-RPC
// message per text frame. The unix control socket speaks the same.
type wsLink struct {
	conn    net.Conn
	r       *bufio.Reader
	wmu     sync.Mutex
	closing bool
	once    sync.Once
	done    chan struct{}
	err     error
}

const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xa
)

// dialLink connects to a and completes the websocket handshake.
func dialLink(ctx context.Context, a Attach) (*wsLink, error) {
	u, err := url.Parse(a.URL)
	if err != nil {
		return nil, fmt.Errorf("attach url: %w", err)
	}
	var conn net.Conn
	host, path := u.Host, u.RequestURI()
	var d net.Dialer
	switch u.Scheme {
	case "unix":
		if u.Host != "" || u.Path == "" {
			return nil, fmt.Errorf("attach url %q: unix:// needs an absolute socket path", a.URL)
		}
		conn, err = d.DialContext(ctx, "unix", u.Path)
		host, path = "localhost", "/"
	case "ws":
		if u.Host == "" {
			return nil, fmt.Errorf("attach url %q: missing host", a.URL)
		}
		addr := u.Host
		if u.Port() == "" {
			addr = net.JoinHostPort(u.Hostname(), "80")
		}
		conn, err = d.DialContext(ctx, "tcp", addr)
	default:
		return nil, fmt.Errorf("attach url %q: want unix://PATH or ws://host:port", a.URL)
	}
	if err != nil {
		return nil, err
	}
	token := ""
	if a.TokenFile != "" {
		b, err := os.ReadFile(a.TokenFile)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("attach token: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	r, err := wsHandshake(conn, host, path, token)
	if err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return &wsLink{conn: conn, r: r, done: make(chan struct{})}, nil
}

// wsHandshake sends the HTTP upgrade and checks the reply; the reader keeps
// any frame bytes that came with it. No Origin header: codex rejects those.
func wsHandshake(conn net.Conn, host, path, token string) (*bufio.Reader, error) {
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	nonce := base64.StdEncoding.EncodeToString(key)
	req := &http.Request{
		Method: http.MethodGet, URL: &url.URL{Path: path}, Host: host, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{
			"Upgrade":               {"websocket"},
			"Connection":            {"Upgrade"},
			"Sec-WebSocket-Key":     {nonce},
			"Sec-WebSocket-Version": {"13"},
		},
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if err := req.Write(conn); err != nil {
		return nil, err
	}
	r := bufio.NewReaderSize(conn, 64<<10)
	resp, err := http.ReadResponse(r, req)
	if err != nil {
		return nil, fmt.Errorf("websocket handshake: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("websocket handshake: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != wsAccept(nonce) {
		return nil, errors.New("websocket handshake: bad Sec-WebSocket-Accept")
	}
	return r, nil
}

func wsAccept(nonce string) string {
	h := sha1.Sum([]byte(nonce + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h[:])
}

// Write sends one message as a masked text frame.
func (l *wsLink) Write(p []byte) (int, error) {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	if l.closing {
		return 0, errors.New("websocket closed")
	}
	if err := writeFrame(l.conn, opText, bytes.TrimSuffix(p, []byte("\n")), true); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Read returns the next data message, answering pings and close on the way.
func (l *wsLink) Read() ([]byte, error) {
	var msg []byte
	for {
		fin, op, payload, err := readFrame(l.r, maxMessage-len(msg))
		if err != nil {
			l.finish(fmt.Errorf("websocket read: %w", err))
			return nil, err
		}
		switch op {
		case opPing:
			l.control(opPong, payload)
		case opPong:
		case opClose:
			l.control(opClose, payload)
			l.finish(io.EOF)
			return nil, io.EOF
		case opText, opBinary:
			if msg != nil {
				l.finish(errors.New("websocket: data frame inside a fragmented message"))
				return nil, l.err
			}
			msg = payload
			if fin {
				return msg, nil
			}
		case opContinuation:
			if msg == nil {
				l.finish(errors.New("websocket: continuation without a message"))
				return nil, l.err
			}
			msg = append(msg, payload...)
			if fin {
				return msg, nil
			}
		default:
			l.finish(fmt.Errorf("websocket: unknown opcode %#x", op))
			return nil, l.err
		}
	}
}

func (l *wsLink) control(op byte, payload []byte) {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	if len(payload) > 125 {
		payload = payload[:125]
	}
	_ = writeFrame(l.conn, op, payload, true)
}

func (l *wsLink) finish(err error) {
	l.once.Do(func() {
		l.err = err
		close(l.done)
	})
}

// Close sends a close frame; the peer's reply ends Read and closes Done.
func (l *wsLink) Close() error {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	if l.closing {
		return nil
	}
	l.closing = true
	return writeFrame(l.conn, opClose, []byte{0x03, 0xe8}, true) // 1000 normal closure
}

func (l *wsLink) Kill() {
	_ = l.conn.Close()
	l.finish(errors.New("websocket closed"))
}

func (l *wsLink) Done() <-chan struct{} { return l.done }

func (l *wsLink) Err() error {
	select {
	case <-l.done:
		return l.err
	default:
		return nil
	}
}

// writeFrame writes one unfragmented frame. Clients must mask.
func writeFrame(w io.Writer, op byte, payload []byte, mask bool) error {
	n := len(payload)
	head := make([]byte, 2, 14)
	head[0] = 0x80 | op
	switch {
	case n < 126:
		head[1] = byte(n)
	case n < 1<<16:
		head[1] = 126
		head = binary.BigEndian.AppendUint16(head, uint16(n))
	default:
		head[1] = 127
		head = binary.BigEndian.AppendUint64(head, uint64(n))
	}
	body := payload
	if mask {
		head[1] |= 0x80
		var key [4]byte
		if _, err := rand.Read(key[:]); err != nil {
			return err
		}
		head = append(head, key[:]...)
		body = make([]byte, n)
		for i := range payload {
			body[i] = payload[i] ^ key[i&3]
		}
	}
	if _, err := w.Write(append(head, body...)); err != nil {
		return err
	}
	return nil
}

// readFrame reads one frame of at most limit bytes, unmasking if masked.
func readFrame(r *bufio.Reader, limit int) (fin bool, op byte, payload []byte, err error) {
	var head [2]byte
	if _, err = io.ReadFull(r, head[:]); err != nil {
		return false, 0, nil, err
	}
	fin = head[0]&0x80 != 0
	op = head[0] & 0x0f
	masked := head[1]&0x80 != 0
	n := uint64(head[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(r, b[:]); err != nil {
			return false, 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(r, b[:]); err != nil {
			return false, 0, nil, err
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > uint64(limit) {
		return false, 0, nil, fmt.Errorf("frame of %d bytes exceeds limit", n)
	}
	var key [4]byte
	if masked {
		if _, err = io.ReadFull(r, key[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(r, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= key[i&3]
		}
	}
	return fin, op, payload, nil
}
