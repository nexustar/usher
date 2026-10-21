package auth

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStore_SetPasswordRoundtrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.IsConfigured() {
		t.Fatal("fresh store should not be configured")
	}
	if err := s.SetPassword("hunter2"); err != nil {
		t.Fatal(err)
	}
	if !s.IsConfigured() {
		t.Fatal("expected configured after SetPassword")
	}
	if !s.Verify("hunter2") {
		t.Fatal("Verify should accept the password we just set")
	}
	if s.Verify("hunter3") {
		t.Fatal("Verify must reject wrong password")
	}
	if s.Verify("") {
		t.Fatal("Verify must reject empty password")
	}
}

func TestStore_SetPasswordPersists(t *testing.T) {
	dir := t.TempDir()
	s1, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.SetPassword("hunter2"); err != nil {
		t.Fatal(err)
	}

	s2, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.IsConfigured() {
		t.Fatal("expected configured after reload")
	}
	if !s2.Verify("hunter2") {
		t.Fatal("Verify must accept the persisted password")
	}
}

func TestStore_SecretPersists(t *testing.T) {
	dir := t.TempDir()
	s1, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	first := append([]byte(nil), s1.secret...)

	s2, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(s2.secret) != string(first) {
		t.Fatal("secret must persist across Load")
	}

	// Sanity check: the on-disk file is 0600.
	info, err := os.Stat(filepath.Join(dir, secretFileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("secret perms = %v, want 0600", info.Mode().Perm())
	}
}

func TestStore_AuthFilePerms(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword("x"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, authFileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("auth.json perms = %v, want 0600", info.Mode().Perm())
	}
}

func TestStore_CookieRoundtrip(t *testing.T) {
	dir := t.TempDir()
	s, _ := Load(dir)
	if err := s.SetPassword("hunter2"); err != nil {
		t.Fatal(err)
	}
	val, err := s.IssueCookie()
	if err != nil {
		t.Fatal(err)
	}
	if !s.VerifyCookie(val) {
		t.Fatal("freshly issued cookie should verify")
	}
	if s.VerifyCookie("") {
		t.Fatal("empty cookie must not verify")
	}
	if s.VerifyCookie(val + "x") {
		t.Fatal("tampered cookie must not verify")
	}
}

func TestStore_CookieInvalidAfterPasswordChange(t *testing.T) {
	dir := t.TempDir()
	s, _ := Load(dir)
	_ = s.SetPassword("a")
	val, _ := s.IssueCookie()
	if !s.VerifyCookie(val) {
		t.Fatal("baseline cookie should verify")
	}
	if err := s.SetPassword("b"); err != nil {
		t.Fatal(err)
	}
	if s.VerifyCookie(val) {
		t.Fatal("old cookie must not verify after password change")
	}
}

func TestStore_CookieInvalidAfterSecretRotation(t *testing.T) {
	dir := t.TempDir()
	s1, _ := Load(dir)
	_ = s1.SetPassword("a")
	val, _ := s1.IssueCookie()

	// Rotate secret on disk; same password, new HMAC key.
	if err := os.Remove(filepath.Join(dir, secretFileName)); err != nil {
		t.Fatal(err)
	}
	s2, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s2.VerifyCookie(val) {
		t.Fatal("old cookie must not verify after secret rotation")
	}
}

func TestStore_IssueCookieRequiresConfigured(t *testing.T) {
	dir := t.TempDir()
	s, _ := Load(dir)
	if _, err := s.IssueCookie(); err == nil {
		t.Fatal("IssueCookie must error before SetPassword")
	}
}

func TestLimiter_AcquireFreshIP(t *testing.T) {
	l := NewLimiter()
	if _, ok := l.Acquire("1.2.3.4"); !ok {
		t.Fatal("fresh IP must be allowed")
	}
}

func TestLimiter_GracePeriod(t *testing.T) {
	l := NewLimiter()
	for i := 0; i < rateLimitGrace; i++ {
		if _, ok := l.Acquire("1.2.3.4"); !ok {
			t.Fatalf("failure %d still inside grace must be allowed", i)
		}
		l.OnFailure("1.2.3.4")
	}
	// 5 failures consumed but no backoff yet; 6th attempt still allowed,
	// fails, *then* a backoff arms before the 7th attempt.
	if _, ok := l.Acquire("1.2.3.4"); !ok {
		t.Fatal("attempt right after grace failures must be allowed")
	}
	l.OnFailure("1.2.3.4")
	if d, ok := l.Acquire("1.2.3.4"); ok || d <= 0 {
		t.Fatalf("attempt after 6th failure must be blocked, got d=%v ok=%v", d, ok)
	}
}

func TestLimiter_BackoffGrowsExponentially(t *testing.T) {
	base := time.Unix(0, 0)
	now := base
	l := &Limiter{entries: map[string]*limEntry{}, now: func() time.Time { return now }}

	// Burn through grace.
	for i := 0; i < rateLimitGrace; i++ {
		l.OnFailure("ip")
	}

	want := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		32 * time.Second,
		60 * time.Second, // capped
		60 * time.Second,
	}
	for i, w := range want {
		l.OnFailure("ip")
		d, ok := l.Acquire("ip")
		if ok {
			t.Fatalf("step %d: must be blocked", i)
		}
		if d != w {
			t.Fatalf("step %d: backoff=%v want %v", i, d, w)
		}
	}
}

func TestLimiter_SuccessResets(t *testing.T) {
	l := NewLimiter()
	for i := 0; i < 10; i++ {
		l.OnFailure("ip")
	}
	if _, ok := l.Acquire("ip"); ok {
		t.Fatal("expected blocked after 10 failures")
	}
	l.OnSuccess("ip")
	if _, ok := l.Acquire("ip"); !ok {
		t.Fatal("OnSuccess must clear backoff")
	}
}

func TestLimiter_GCDropsStaleEntries(t *testing.T) {
	base := time.Unix(0, 0)
	now := base
	l := &Limiter{entries: map[string]*limEntry{}, now: func() time.Time { return now }}
	l.OnFailure("ip")
	now = base.Add(2 * rateLimitEntryStale)
	if _, ok := l.Acquire("ip"); !ok {
		t.Fatal("stale entry must be GC'd and attempt allowed")
	}
	l.mu.Lock()
	_, exists := l.entries["ip"]
	l.mu.Unlock()
	if exists {
		t.Fatal("stale entry must be removed by Acquire's gc")
	}
}

func TestStore_LoadIgnoresMissingAuthFile(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.IsConfigured() {
		t.Fatal("missing auth.json should mean unconfigured, not error")
	}
}

func TestStore_LoadRejectsUnknownAlgo(t *testing.T) {
	dir := t.TempDir()
	bad := `{"algo":"sha256","time":0,"memory":0,"threads":0,"key_len":0,"salt":"","hash":""}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, authFileName), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("Load must reject unknown algo")
	}
}
