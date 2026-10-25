// Package push sends Web Push messages (RFC 8030) to browsers that
// subscribed, so a phone hears about a thread with starcode closed. The
// payload is encrypted for the subscription (RFC 8291, aes128gcm) and the
// request is signed with this instance's VAPID key (RFC 8292). It is all
// standard library: ECDH and ECDSA on P-256, HKDF and AES-GCM.
package push

import (
	"bytes"
	"context"
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
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

// ErrGone is a subscription the push service no longer knows: the
// browser unsubscribed or the app was removed. Drop it.
var ErrGone = errors.New("push subscription is gone")

// Subscription is what PushManager.subscribe gives the page.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// Message is what the service worker shows (static/sw.js).
type Message struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	// Tag replaces an earlier notification with the same tag, so a
	// thread has one on the lock screen at a time.
	Tag string `json:"tag,omitempty"`
}

// Sender holds the VAPID key.
type Sender struct {
	key *ecdsa.PrivateKey
	// Subject is the contact the push services may use (mailto: or
	// https:); Apple refuses requests without one.
	Subject string
	Client  *http.Client
}

// Load reads the VAPID key at path, making one on first use. It must stay
// the same: every subscription is bound to it.
func Load(path string) (*Sender, error) {
	s := &Sender{Subject: "mailto:starcode@example.com", Client: &http.Client{Timeout: 30 * time.Second}}
	if b, err := os.ReadFile(path); err == nil {
		blk, _ := pem.Decode(b)
		if blk == nil {
			return nil, fmt.Errorf("%s: no PEM block", path)
		}
		if s.key, err = x509.ParseECPrivateKey(blk.Bytes); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return s, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	s.key = key
	return s, nil
}

// PublicKey is the applicationServerKey for PushManager.subscribe,
// base64url without padding.
func (s *Sender) PublicKey() string {
	pub, _ := s.key.PublicKey.ECDH()
	return b64.EncodeToString(pub.Bytes())
}

var b64 = base64.RawURLEncoding

// decode64 reads base64url with or without padding, which browsers vary on.
func decode64(s string) ([]byte, error) {
	for len(s)%4 != 0 {
		s += "="
	}
	return base64.URLEncoding.DecodeString(s)
}

// Send delivers m to sub. urgent asks the push service to wake the device
// now ("high"); the rest may wait for a better moment.
func (s *Sender) Send(ctx context.Context, sub Subscription, m Message, urgent bool) error {
	u, err := url.Parse(sub.Endpoint)
	if err != nil || u.Scheme != "https" {
		return fmt.Errorf("bad push endpoint %q", sub.Endpoint)
	}
	uaPub, err := decode64(sub.Keys.P256dh)
	if err != nil {
		return fmt.Errorf("p256dh: %w", err)
	}
	auth, err := decode64(sub.Keys.Auth)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	payload, _ := json.Marshal(m)
	salt := make([]byte, 16)
	rand.Read(salt)
	as, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	body, err := encrypt(payload, uaPub, auth, as, salt)
	if err != nil {
		return err
	}
	jwt, err := s.token(u.Scheme+"://"+u.Host, time.Now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(24*3600))
	req.Header.Set("Authorization", "vapid t="+jwt+", k="+s.PublicKey())
	if urgent {
		req.Header.Set("Urgency", "high")
	} else {
		req.Header.Set("Urgency", "normal")
	}
	if m.Tag != "" {
		// The service replaces a message still waiting with the same
		// topic (32 base64url characters at most).
		req.Header.Set("Topic", topic(m.Tag))
	}
	res, err := s.Client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusGone:
		return ErrGone
	case res.StatusCode >= 300:
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("push service: %s: %s", res.Status, bytes.TrimSpace(msg))
	}
	return nil
}

func topic(tag string) string {
	h := sha256.Sum256([]byte(tag))
	return b64.EncodeToString(h[:])[:32]
}

// token is the VAPID JWT (ES256) for the push service at aud.
func (s *Sender) token(aud string, now time.Time) (string, error) {
	head := b64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, _ := json.Marshal(map[string]any{"aud": aud, "exp": now.Add(12 * time.Hour).Unix(), "sub": s.Subject})
	unsigned := head + "." + b64.EncodeToString(claims)
	h := sha256.Sum256([]byte(unsigned))
	r, sv, err := ecdsa.Sign(rand.Reader, s.key, h[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	sv.FillBytes(sig[32:])
	return unsigned + "." + b64.EncodeToString(sig), nil
}

// recordSize is the rs of the one record a message fits in.
const recordSize = 4096

// encrypt is RFC 8291: a key shared between the throwaway key as and the
// browser's key, mixed with the browser's auth secret, encrypts payload as
// one aes128gcm record (RFC 8188) whose header carries as's public key.
func encrypt(payload, uaPub, auth []byte, as *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	if len(payload) > recordSize-16-1-86 {
		return nil, errors.New("push payload too big")
	}
	ua, err := ecdh.P256().NewPublicKey(uaPub)
	if err != nil {
		return nil, fmt.Errorf("p256dh: %w", err)
	}
	shared, err := as.ECDH(ua)
	if err != nil {
		return nil, err
	}
	asPub := as.PublicKey().Bytes()
	prkKey, err := hkdf.Extract(sha256.New, shared, auth)
	if err != nil {
		return nil, err
	}
	ikm, err := hkdf.Expand(sha256.New, prkKey, "WebPush: info\x00"+string(uaPub)+string(asPub), 32)
	if err != nil {
		return nil, err
	}
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
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// 0x02 ends the last (only) record; no padding.
	plain := append(append([]byte{}, payload...), 2)
	out := make([]byte, 0, 16+4+1+len(asPub)+len(plain)+16)
	out = append(out, salt...)
	out = binary.BigEndian.AppendUint32(out, recordSize)
	out = append(out, byte(len(asPub)))
	out = append(out, asPub...)
	return gcm.Seal(out, nonce, plain, nil), nil
}
