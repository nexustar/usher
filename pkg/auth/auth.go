// Package auth implements usher's password-based web UI auth: argon2id
// password hashing, an HMAC-signed stateless cookie tied to the current
// password hash (so changing password kicks every device), and a per-IP
// exponential-backoff rate limiter for /login.
//
// All on-disk artifacts live under the usher data dir with mode 0600:
//
//	auth.json — argon2id parameters + salt + hash (absent ⇒ auth disabled)
//	secret    — 32-byte random HMAC key, generated on first run
//
// Auth is disabled iff auth.json is absent. The cookie value is
// base64url(HMAC(secret, hash_bytes)); rotating either the password
// (which rotates the hash) or the secret invalidates every old cookie
// without any server-side session table.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

	// 30 days. Cookie has no server-side expiry; this is just how long
	// browsers keep it without a fresh login.
	cookieMaxAge = 30 * 24 * 60 * 60

	rateLimitGrace      = 5
	rateLimitMaxBackoff = 60 * time.Second
	rateLimitEntryStale = time.Hour
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

	mu     sync.RWMutex
	hash   *Hash  // nil ⇒ auth disabled
	secret []byte // HMAC key, always non-nil after Load

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
	s := &Store{dataDir: dataDir, Limiter: NewLimiter()}

	h, err := readHash(filepath.Join(dataDir, authFileName))
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
	if err := writeHash(filepath.Join(s.dataDir, authFileName), h); err != nil {
		return err
	}
	s.mu.Lock()
	s.hash = h
	s.mu.Unlock()
	return nil
}

// Verify reports whether plaintext matches the stored hash. Returns false
// (without error) when auth is not configured. Constant-time compare.
func (s *Store) Verify(plaintext string) bool {
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

// IssueCookie returns the cookie value for the current password hash.
func (s *Store) IssueCookie() (string, error) {
	mac, err := s.macForHash()
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(mac), nil
}

// VerifyCookie reports whether value is a valid cookie for the current
// password hash. Constant-time compare.
func (s *Store) VerifyCookie(value string) bool {
	if value == "" {
		return false
	}
	want, err := s.macForHash()
	if err != nil {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(want, got) == 1
}

func (s *Store) macForHash() ([]byte, error) {
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
	return mac.Sum(nil), nil
}

// NewSessionCookie builds a Set-Cookie payload for issuing a fresh session.
func (s *Store) NewSessionCookie(value string) *http.Cookie {
	return &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   cookieMaxAge,
	}
}

// ClearCookie builds a Set-Cookie payload that deletes the session cookie
// on the client side.
func (s *Store) ClearCookie() *http.Cookie {
	return &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	}
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// --- rate limiter --------------------------------------------------------

// Limiter is a per-IP exponential-backoff limiter for /login. The first 5
// failures incur no delay; failure #6 waits 1s before the next attempt,
// #7 waits 2s, #8 waits 4s, capped at 60s. A successful Acquire+OnSuccess
// pair resets the IP's counter.
type Limiter struct {
	mu      sync.Mutex
	entries map[string]*limEntry
	now     func() time.Time
}

type limEntry struct {
	failures    int
	nextAllowed time.Time
	lastUpdated time.Time
}

func NewLimiter() *Limiter {
	return &Limiter{entries: map[string]*limEntry{}, now: time.Now}
}

// Acquire returns (retryAfter, true) if the IP may attempt now and
// (retryAfter, false) if it is still in backoff. The caller must invoke
// OnSuccess or OnFailure after performing the verification.
func (l *Limiter) Acquire(ip string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gcLocked()
	e, ok := l.entries[ip]
	if !ok {
		return 0, true
	}
	if d := e.nextAllowed.Sub(l.now()); d > 0 {
		return d, false
	}
	return 0, true
}

// OnFailure records a failed attempt and arms a backoff once past the grace
// window.
func (l *Limiter) OnFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[ip]
	if e == nil {
		e = &limEntry{}
		l.entries[ip] = e
	}
	e.failures++
	e.lastUpdated = l.now()
	if e.failures > rateLimitGrace {
		shift := e.failures - rateLimitGrace - 1
		if shift > 6 {
			shift = 6
		}
		d := time.Second * time.Duration(1<<shift)
		if d > rateLimitMaxBackoff {
			d = rateLimitMaxBackoff
		}
		e.nextAllowed = l.now().Add(d)
	}
}

// OnSuccess clears the IP's counter.
func (l *Limiter) OnSuccess(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, ip)
}

func (l *Limiter) gcLocked() {
	cutoff := l.now().Add(-rateLimitEntryStale)
	for ip, e := range l.entries {
		if e.lastUpdated.Before(cutoff) {
			delete(l.entries, ip)
		}
	}
}

// ClientIP extracts an IP from req.RemoteAddr. Usher does not run behind a
// trusted reverse proxy in v0.1, so we deliberately ignore X-Forwarded-For
// et al. to avoid header-injection bypasses.
func ClientIP(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}
