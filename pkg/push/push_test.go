package push

import (
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

// TestEncryptPayload_RFC8291 reproduces the worked example from RFC 8291 §5
// exactly. Pinning the salt and the application-server ephemeral key (the only
// random inputs) makes the entire derivation a known-answer test: if any HKDF
// step, the key_info layout, the padding delimiter, or the header framing is
// wrong, the bytes won't match.
func TestEncryptPayload_RFC8291(t *testing.T) {
	plaintext := mustB64(t, "V2hlbiBJIGdyb3cgdXAsIEkgd2FudCB0byBiZSBhIHdhdGVybWVsb24")
	authSecret := mustB64(t, "BTBZMqHH6r4Tts7J_aSIgg")
	uaPublic := mustB64(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	asPrivBytes := mustB64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw")
	salt := mustB64(t, "DGv6ra1nlYgDCS1FRnbzlw")
	want := mustB64(t, "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN")

	asPriv, err := ecdh.P256().NewPrivateKey(asPrivBytes)
	if err != nil {
		t.Fatalf("as priv: %v", err)
	}

	got, err := encryptPayload(plaintext, uaPublic, authSecret, asPriv, salt)
	if err != nil {
		t.Fatalf("encryptPayload: %v", err)
	}
	if !strings.EqualFold(base64.RawURLEncoding.EncodeToString(got), base64.RawURLEncoding.EncodeToString(want)) {
		t.Fatalf("ciphertext mismatch\n got %x\nwant %x", got, want)
	}
}

// TestEncryptPayload_RandomKeys checks the non-pinned path runs end to end with
// freshly generated ephemeral keys and a real subscription's key shape.
func TestEncryptPayload_RandomKeys(t *testing.T) {
	uaPriv, err := ecdh.P256().GenerateKey(randReader{})
	if err != nil {
		t.Fatal(err)
	}
	asPriv, err := ecdh.P256().GenerateKey(randReader{})
	if err != nil {
		t.Fatal(err)
	}
	salt := make([]byte, 16)
	body, err := encryptPayload([]byte(`{"hello":"world"}`), uaPriv.PublicKey().Bytes(), make([]byte, 16), asPriv, salt)
	if err != nil {
		t.Fatalf("encryptPayload: %v", err)
	}
	// header = salt(16) + rs(4) + idlen(1) + as_public(65) = 86, plus ciphertext.
	if len(body) <= 86 {
		t.Fatalf("body too short: %d", len(body))
	}
	if body[20] != 65 {
		t.Fatalf("keyid length byte = %d, want 65", body[20])
	}
}

func TestVAPIDAuthHeader(t *testing.T) {
	dir := t.TempDir()
	keys, err := loadOrCreateVAPID(filepath.Join(dir, "vapid.json"))
	if err != nil {
		t.Fatal(err)
	}
	hdr, err := keys.authHeader("https://fcm.googleapis.com/fcm/send/abc123", vapidSubscriber, time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hdr, "vapid t=") || !strings.Contains(hdr, ", k=") {
		t.Fatalf("bad header: %q", hdr)
	}
	jwt := strings.TrimPrefix(strings.SplitN(hdr, ", k=", 2)[0], "vapid t=")
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts", len(parts))
	}
	var head struct{ Typ, Alg string }
	if err := json.Unmarshal(mustB64(t, parts[0]), &head); err != nil {
		t.Fatal(err)
	}
	if head.Alg != "ES256" {
		t.Fatalf("alg = %q", head.Alg)
	}
	var claims struct {
		Aud, Sub string
		Exp      int64
	}
	if err := json.Unmarshal(mustB64(t, parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Aud != "https://fcm.googleapis.com" {
		t.Fatalf("aud = %q", claims.Aud)
	}
	if sig := mustB64(t, parts[2]); len(sig) != 64 {
		t.Fatalf("sig len = %d, want 64", len(sig))
	}
}

func TestVAPIDKeysStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vapid.json")
	a, err := loadOrCreateVAPID(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := loadOrCreateVAPID(path)
	if err != nil {
		t.Fatal(err)
	}
	if a.publicKeyB64() != b.publicKeyB64() {
		t.Fatal("VAPID public key changed across loads; existing subscriptions would break")
	}
}

func TestStorePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subs.json")
	s := newStore(path)
	var sub Subscription
	sub.Endpoint = "https://push.example/abc"
	sub.Keys.P256dh = "p256"
	sub.Keys.Auth = "auth"
	s.add(sub)
	s.add(sub) // idempotent

	if got := newStore(path).all(); len(got) != 1 || got[0].Endpoint != sub.Endpoint {
		t.Fatalf("reload = %+v", got)
	}

	s.remove(sub.Endpoint)
	if got := newStore(path).all(); len(got) != 0 {
		t.Fatalf("after remove = %+v", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("store file missing: %v", err)
	}
}

// randReader is a deterministic-enough reader for tests that only need
// well-formed keys, avoiding a crypto/rand import here.
type randReader struct{}

func (randReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(i*7 + 1)
	}
	return len(p), nil
}
