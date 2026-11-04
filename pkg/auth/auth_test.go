package auth

import (
	"crypto/tls"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

func TestClearPassword(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	if removed, err := ClearPassword(dir); err != nil || !removed {
		t.Fatalf("ClearPassword = %v, %v; want true, nil", removed, err)
	}
	if !s.IsConfigured() {
		t.Error("running store dropped its hash; want it kept until restart")
	}
	s2, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s2.IsConfigured() {
		t.Error("reloaded store still configured after clear")
	}
	if removed, err := ClearPassword(dir); err != nil || removed {
		t.Fatalf("second ClearPassword = %v, %v; want false, nil", removed, err)
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
	if _, ok := l.Acquire(); !ok {
		t.Fatal("fresh IP must be allowed")
	}
}

func TestLimiter_GracePeriod(t *testing.T) {
	l := NewLimiter()
	for i := 0; i < rateLimitGrace; i++ {
		if _, ok := l.Acquire(); !ok {
			t.Fatalf("failure %d still inside grace must be allowed", i)
		}
		l.OnFailure()
	}
	// 5 failures consumed but no backoff yet; 6th attempt still allowed,
	// fails, *then* a backoff arms before the 7th attempt.
	if _, ok := l.Acquire(); !ok {
		t.Fatal("attempt right after grace failures must be allowed")
	}
	l.OnFailure()
	if d, ok := l.Acquire(); ok || d <= 0 {
		t.Fatalf("attempt after 6th failure must be blocked, got d=%v ok=%v", d, ok)
	}
}

func TestLimiter_BackoffGrowsExponentially(t *testing.T) {
	base := time.Unix(0, 0)
	now := base
	l := &Limiter{now: func() time.Time { return now }}

	// Burn through grace.
	for i := 0; i < rateLimitGrace; i++ {
		l.OnFailure()
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
		l.OnFailure()
		d, ok := l.Acquire()
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
		l.OnFailure()
	}
	if _, ok := l.Acquire(); ok {
		t.Fatal("expected blocked after 10 failures")
	}
	l.OnSuccess()
	if _, ok := l.Acquire(); !ok {
		t.Fatal("OnSuccess must clear backoff")
	}
}

func TestLimiter_OneAttemptAtATime(t *testing.T) {
	l := NewLimiter()
	if _, ok := l.Acquire(); !ok {
		t.Fatal("first attempt must be allowed")
	}
	if d, ok := l.Acquire(); ok || d <= 0 {
		t.Fatalf("second attempt must wait for the first, got d=%v ok=%v", d, ok)
	}
	l.OnFailure()
	if _, ok := l.Acquire(); !ok {
		t.Fatal("attempt must be allowed once the first finished")
	}
}

func TestLimiter_StaleFailuresForgotten(t *testing.T) {
	base := time.Unix(1000, 0)
	now := base
	l := &Limiter{now: func() time.Time { return now }}
	for i := 0; i < 10; i++ {
		l.OnFailure()
	}
	now = base.Add(2 * rateLimitStale)
	if _, ok := l.Acquire(); !ok {
		t.Fatal("stale failures must not block")
	}
	if l.failures != 0 {
		t.Fatalf("failures = %d, want 0", l.failures)
	}
}

// A second process is how `usher set-password` reaches a running server.
func TestStore_PicksUpPasswordFromAnotherProcess(t *testing.T) {
	dir := t.TempDir()
	server, _ := Load(dir)
	if server.IsConfigured() {
		t.Fatal("fresh store should not be configured")
	}
	cli, _ := Load(dir)
	if err := cli.SetPassword("first-password"); err != nil {
		t.Fatal(err)
	}
	if !server.IsConfigured() || !server.Verify("first-password") {
		t.Fatal("running store must pick up a first password")
	}
	cookie, err := server.IssueCookie()
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.SetPassword("second-password"); err != nil {
		t.Fatal(err)
	}
	if server.Verify("first-password") || !server.Verify("second-password") {
		t.Fatal("running store must pick up a changed password")
	}
	if server.VerifyCookie(cookie) {
		t.Fatal("running store must reject cookies of the old password")
	}
}

func TestStore_BadAuthFileKeepsLastHash(t *testing.T) {
	dir := t.TempDir()
	s, _ := Load(dir)
	if err := s.SetPassword("hunter2"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, authFileName)
	if err := os.WriteFile(path, []byte("{half a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !s.IsConfigured() || !s.Verify("hunter2") {
		t.Fatal("unparsable auth.json must not change the password")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if !s.IsConfigured() {
		t.Fatal("removing auth.json must not turn auth off in a running store")
	}
}

func TestStore_CookieExpires(t *testing.T) {
	s, _ := Load(t.TempDir())
	if err := s.SetPassword("hunter2"); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_000_000, 0)
	s.now = func() time.Time { return now }
	val, err := s.IssueCookie()
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(cookieMaxAge*time.Second - time.Second)
	if !s.VerifyCookie(val) {
		t.Fatal("cookie must verify just inside its lifetime")
	}
	now = now.Add(time.Second)
	if s.VerifyCookie(val) {
		t.Fatal("cookie must not verify past its lifetime")
	}
}

func TestStore_CookieIssueTimeIsSigned(t *testing.T) {
	s, _ := Load(t.TempDir())
	if err := s.SetPassword("hunter2"); err != nil {
		t.Fatal(err)
	}
	val, _ := s.IssueCookie()
	_, sig, _ := strings.Cut(val, ".")
	forged := strconv.FormatInt(time.Now().Unix()+1000, 10) + "." + sig
	if s.VerifyCookie(forged) {
		t.Fatal("a changed issue time must not verify")
	}
}

func TestSessionCookie_SecureOverHTTPS(t *testing.T) {
	s, _ := Load(t.TempDir())
	cases := []struct {
		proto string
		tls   bool
		want  bool
	}{
		{want: false},
		{proto: "http", want: false},
		{proto: "https", want: true},
		{proto: "https,http", want: true},
		{tls: true, want: true},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "/login", nil)
		if c.proto != "" {
			r.Header.Set("X-Forwarded-Proto", c.proto)
		}
		if c.tls {
			r.TLS = &tls.ConnectionState{}
		}
		if got := s.NewSessionCookie(r, "v").Secure; got != c.want {
			t.Errorf("proto=%q tls=%v: Secure=%v, want %v", c.proto, c.tls, got, c.want)
		}
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
