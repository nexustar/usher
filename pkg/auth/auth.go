// Package auth implements usher's password-based web UI auth: argon2id
// password hashing, an HMAC-signed stateless cookie tied to the current
// password hash (so changing password kicks every device), and a global
// exponential-backoff rate limiter for /login.
//
// All on-disk artifacts live under the usher data dir with mode 0600:
//
//	auth.json — argon2id parameters + salt + hash (absent ⇒ auth disabled)
//	secret    — 32-byte random HMAC key, generated on first run
//
// Auth is disabled iff auth.json is absent at startup. The running server
// picks up a new auth.json on the next request, so `usher set-password` takes
// effect without a restart; a file removed underneath it keeps the last hash.
// The cookie value is <unix issue time>.base64url(HMAC(secret, hash_bytes ||
// issue time)); rotating either the password (which rotates the hash) or the
// secret invalidates every old cookie without any server-side session table.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

const (
	authFileName   = "auth.json"
	secretFileName = "secret"

	// CookieName is the cookie key checked by the web middleware.
	CookieName = "usher_auth"

	argonTime    uint32 = 2
	argonMemory  uint32 = 64 * 1024 // 64 MiB
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32
	saltLen             = 16
	secretLen           = 32

	// MinPasswordLen is what `usher set-password` accepts, in characters.
	MinPasswordLen = 8

	// 30 days, in seconds, enforced by the server as well as the browser.
	cookieMaxAge = 30 * 24 * 60 * 60

	rateLimitGrace      = 5
	rateLimitMaxBackoff = 60 * time.Second
	rateLimitStale      = time.Hour
)

// Hash is the on-disk argon2id record.
type Hash struct {
	Algo    string `json:"algo"`
	Time    uint32 `json:"time"`
	Memory  uint32 `json:"memory"`
	Threads uint8  `json:"threads"`
	KeyLen  uint32 `json:"key_len"`
	Salt    string `json:"salt"`
	Hash    string `json:"hash"`
}

// Store owns the auth state for one running server.
type Store struct {
	dataDir string

	now func() time.Time

	mu       sync.RWMutex
	hash     *Hash       // nil ⇒ auth disabled
	hashFile os.FileInfo // auth.json as last read; nil ⇒ absent
	secret   []byte      // HMAC key, always non-nil after Load

	// Limiter gates /login attempts. Callers acquire before verifying and
	// report success/failure after.
	Limiter *Limiter
}

// Load reads auth.json (if present) and the HMAC secret (generates one if
// absent), then returns a ready-to-use Store. Never creates auth.json —
// that is the SetPassword path's job.
func Load(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	s := &Store{dataDir: dataDir, Limiter: NewLimiter(), now: time.Now}

	path := filepath.Join(dataDir, authFileName)
	s.hashFile, _ = os.Stat(path) // before the read: a later write re-reads
	h, err := readHash(path)
	if err != nil {
		return nil, err
	}
	s.hash = h

	secret, err := loadOrCreateSecret(filepath.Join(dataDir, secretFileName))
	if err != nil {
		return nil, err
	}
	s.secret = secret
	return s, nil
}

// IsConfigured reports whether a password has been set.
func (s *Store) IsConfigured() bool {
	s.refresh()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hash != nil
}

// SetPassword hashes plaintext with argon2id and writes auth.json (0600).
// The new hash also invalidates every previously-issued cookie because
// each cookie is keyed by the hash bytes.
func (s *Store) SetPassword(plaintext string) error {
	if plaintext == "" {
		return errors.New("password must not be empty")
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	key := argon2.IDKey([]byte(plaintext), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	h := &Hash{
		Algo:    "argon2id",
		Time:    argonTime,
		Memory:  argonMemory,
		Threads: argonThreads,
		KeyLen:  argonKeyLen,
		Salt:    base64.StdEncoding.EncodeToString(salt),
		Hash:    base64.StdEncoding.EncodeToString(key),
	}
	path := filepath.Join(s.dataDir, authFileName)
	if err := writeHash(path, h); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.hash = h
	s.hashFile = info
	s.mu.Unlock()
	return nil
}

// refresh picks up `usher set-password`; a vanished or bad file keeps the last hash.
func (s *Store) refresh() {
	path := filepath.Join(s.dataDir, authFileName)
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	s.mu.RLock()
	seen := s.hashFile
	s.mu.RUnlock()
	if seen != nil && seen.Size() == info.Size() && seen.ModTime().Equal(info.ModTime()) {
		return
	}
	h, err := readHash(path)
	if err != nil {
		slog.Warn("ignoring unreadable auth file", "err", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hashFile = info // a bad file is retried when it next changes, not per request
	if h != nil {
		s.hash = h
	}
}

// Verify reports whether plaintext matches the stored hash. Returns false
// (without error) when auth is not configured. Constant-time compare.
func (s *Store) Verify(plaintext string) bool {
	s.refresh()
	s.mu.RLock()
	h := s.hash
	s.mu.RUnlock()
	if h == nil {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(h.Salt)
	if err != nil {
		return false
	}
	expected, err := base64.StdEncoding.DecodeString(h.Hash)
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(plaintext), salt, h.Time, h.Memory, h.Threads, h.KeyLen)
	return subtle.ConstantTimeCompare(expected, got) == 1
}

// IssueCookie returns a cookie value for the current password hash, issued now.
func (s *Store) IssueCookie() (string, error) {
	issued := s.now().Unix()
	mac, err := s.mac(issued)
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(issued, 10) + "." + base64.RawURLEncoding.EncodeToString(mac), nil
}

// VerifyCookie reports whether value is an unexpired cookie for the current password hash.
func (s *Store) VerifyCookie(value string) bool {
	stamp, sig, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	issued, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return false
	}
	want, err := s.mac(issued)
	if err != nil {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(want, got) == 1 && s.now().Unix()-issued < cookieMaxAge
}

func (s *Store) mac(issued int64) ([]byte, error) {
	s.refresh()
	s.mu.RLock()
	h := s.hash
	secret := s.secret
	s.mu.RUnlock()
	if h == nil {
		return nil, errors.New("auth not configured")
	}
	hashBytes, err := base64.StdEncoding.DecodeString(h.Hash)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(hashBytes)
	mac.Write(binary.BigEndian.AppendUint64(nil, uint64(issued)))
	return mac.Sum(nil), nil
}

// NewSessionCookie builds a Set-Cookie payload for issuing a fresh session.
func (s *Store) NewSessionCookie(r *http.Request, value string) *http.Cookie {
	return &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   overHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   cookieMaxAge,
	}
}

// ClearCookie builds a Set-Cookie payload that deletes the session cookie
// on the client side.
func (s *Store) ClearCookie(r *http.Request) *http.Cookie {
	return &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   overHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	}
}

// overHTTPS is not unconditional because of plain-HTTP LAN access; forging it only hurts the forger.
func overHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

// --- secret + hash persistence -------------------------------------------

func loadOrCreateSecret(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		secret, decErr := hex.DecodeString(strings.TrimSpace(string(raw)))
		if decErr != nil || len(secret) < secretLen {
			return nil, fmt.Errorf("secret file %s is corrupt", path)
		}
		return secret, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	secret := make([]byte, secretLen)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(secret)+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	return secret, nil
}

func readHash(path string) (*Hash, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var h Hash
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if h.Algo != "argon2id" {
		return nil, fmt.Errorf("%s: unsupported algo %q", path, h.Algo)
	}
	return &h, nil
}

func writeHash(path string, h *Hash) error {
	raw, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Atomic, so a running server never reads half a file. CreateTemp is 0600.
	f, err := os.CreateTemp(dir, authFileName+".*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// --- rate limiter --------------------------------------------------------

// Limiter admits one /login attempt at a time, with global backoff.
// The first 5 failures are free, then 1s, 2s, 4s up to 60s; a success or a quiet hour resets it.
type Limiter struct {
	mu          sync.Mutex
	inFlight    bool
	failures    int
	nextAllowed time.Time
	lastFailure time.Time
	now         func() time.Time
}

func NewLimiter() *Limiter {
	return &Limiter{now: time.Now}
}

// Acquire reserves an attempt, to be ended by OnSuccess or OnFailure, or returns how long to wait.
func (l *Limiter) Acquire() (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.now().Sub(l.lastFailure) > rateLimitStale {
		l.failures = 0
		l.nextAllowed = time.Time{}
	}
	if l.inFlight {
		return time.Second, false
	}
	if d := l.nextAllowed.Sub(l.now()); d > 0 {
		return d, false
	}
	l.inFlight = true
	return 0, true
}

// OnFailure records a failed attempt and arms a backoff once past the grace window.
func (l *Limiter) OnFailure() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inFlight = false
	l.failures++
	l.lastFailure = l.now()
	if l.failures > rateLimitGrace {
		shift := l.failures - rateLimitGrace - 1
		if shift > 6 {
			shift = 6
		}
		d := time.Second * time.Duration(1<<shift)
		if d > rateLimitMaxBackoff {
			d = rateLimitMaxBackoff
		}
		l.nextAllowed = l.now().Add(d)
	}
}

// OnSuccess clears the counter.
func (l *Limiter) OnSuccess() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inFlight = false
	l.failures = 0
	l.nextAllowed = time.Time{}
}

// ClientIP is req.RemoteAddr's IP, for the log; clients can forge X-Forwarded-For.
func ClientIP(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}
