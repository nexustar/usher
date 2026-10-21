package push

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// recordSize is the aes128gcm record size advertised in the encryption header.
// Our payloads are a few hundred bytes — far below any push service's ~4KB cap —
// so a single record always suffices.
const recordSize = 4096

// encryptPayload implements Message Encryption for Web Push (RFC 8291) with the
// aes128gcm content encoding (RFC 8188). It returns the full request body:
// header (salt ‖ record-size ‖ keyid-len ‖ as_public) followed by the AEAD
// ciphertext.
//
// salt and asPriv (the application-server ephemeral key) are passed in rather
// than generated here so the RFC 8291 §5 worked example can be reproduced
// exactly in tests; send() generates fresh random ones per message.
func encryptPayload(plaintext, uaPublic, authSecret []byte, asPriv *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	uaKey, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("client p256dh: %w", err)
	}
	shared, err := asPriv.ECDH(uaKey)
	if err != nil {
		return nil, fmt.Errorf("ecdh: %w", err)
	}
	asPublic := asPriv.PublicKey().Bytes()

	// RFC 8291 §3.4: derive the input keying material from the ECDH secret and
	// the subscription's auth secret, binding both public keys into the info.
	keyInfo := make([]byte, 0, len("WebPush: info")+1+len(uaPublic)+len(asPublic))
	keyInfo = append(keyInfo, []byte("WebPush: info")...)
	keyInfo = append(keyInfo, 0x00)
	keyInfo = append(keyInfo, uaPublic...)
	keyInfo = append(keyInfo, asPublic...)

	prkKey, err := hkdf.Extract(sha256.New, shared, authSecret)
	if err != nil {
		return nil, err
	}
	ikm, err := hkdf.Expand(sha256.New, prkKey, string(keyInfo), 32)
	if err != nil {
		return nil, err
	}

	// RFC 8188 §2.1: salt-seeded PRK, then the CEK and nonce.
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// Single, final record: append the 0x02 last-record delimiter (RFC 8188 §2),
	// no further padding needed.
	padded := append(append([]byte{}, plaintext...), 0x02)
	ciphertext := aead.Seal(nil, nonce, padded, nil)

	header := make([]byte, 0, len(salt)+4+1+len(asPublic)+len(ciphertext))
	header = append(header, salt...)
	var rs [4]byte
	binary.BigEndian.PutUint32(rs[:], recordSize)
	header = append(header, rs[:]...)
	header = append(header, byte(len(asPublic)))
	header = append(header, asPublic...)
	return append(header, ciphertext...), nil
}

// vapidKeys is usher's VAPID identity: a stable P-256 keypair the push service
// uses to recognise this application server across messages. pub is the 65-byte
// uncompressed point, shared with browsers as the applicationServerKey.
type vapidKeys struct {
	priv *ecdsa.PrivateKey
	pub  []byte
}

type vapidFile struct {
	Private string `json:"private"` // base64 (std) PKCS#8 DER
}

// loadOrCreateVAPID reads the VAPID keypair from path, generating and
// persisting a fresh one on first run. The key must be stable: rotating it
// silently invalidates every existing browser subscription.
func loadOrCreateVAPID(path string) (*vapidKeys, error) {
	if data, err := os.ReadFile(path); err == nil {
		var f vapidFile
		if err := json.Unmarshal(data, &f); err == nil && f.Private != "" {
			der, err := base64.StdEncoding.DecodeString(f.Private)
			if err == nil {
				if key, err := x509.ParsePKCS8PrivateKey(der); err == nil {
					if priv, ok := key.(*ecdsa.PrivateKey); ok {
						return newVAPIDKeys(priv)
					}
				}
			}
		}
		// Fall through and regenerate on any corruption.
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(vapidFile{Private: base64.StdEncoding.EncodeToString(der)})
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, err
	}
	return newVAPIDKeys(priv)
}

func newVAPIDKeys(priv *ecdsa.PrivateKey) (*vapidKeys, error) {
	ecdhPub, err := priv.PublicKey.ECDH()
	if err != nil {
		return nil, err
	}
	return &vapidKeys{priv: priv, pub: ecdhPub.Bytes()}, nil
}

// publicKeyB64 is the applicationServerKey browsers pass to pushManager.subscribe.
func (k *vapidKeys) publicKeyB64() string {
	return base64.RawURLEncoding.EncodeToString(k.pub)
}

// authHeader builds the VAPID Authorization header (RFC 8292): a signed JWT
// asserting usher's identity to the push service handling endpoint. The
// audience is the endpoint's origin; the token is short-lived.
func (k *vapidKeys) authHeader(endpoint, subscriber string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	aud := u.Scheme + "://" + u.Host

	header := base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims := base64.RawURLEncoding.EncodeToString([]byte(
		fmt.Sprintf(`{"aud":%q,"exp":%d,"sub":%q}`, aud, now.Add(12*time.Hour).Unix(), subscriber)))
	signingInput := header + "." + claims

	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, k.priv, digest[:])
	if err != nil {
		return "", err
	}
	// ES256 signature is the raw 32-byte r ‖ s, not the ASN.1 DER form.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	jwt := signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)

	return "vapid t=" + jwt + ", k=" + k.publicKeyB64(), nil
}

// b64urlDecode tolerates padded/unpadded, URL- or standard-alphabet base64 —
// browsers vary in how they encode subscription keys.
func b64urlDecode(s string) ([]byte, error) {
	s = strings.NewReplacer("+", "-", "/", "_").Replace(s)
	s = strings.TrimRight(s, "=")
	return base64.RawURLEncoding.DecodeString(s)
}
