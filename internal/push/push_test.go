package push

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := decode64(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The example in RFC 8291, appendix A.
func TestEncryptRFC8291(t *testing.T) {
	as, err := ecdh.P256().NewPrivateKey(mustB64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := encrypt([]byte("When I grow up, I want to be a watermelon"),
		mustB64(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"),
		mustB64(t, "BTBZMqHH6r4Tts7J_aSIgg"), as, mustB64(t, "DGv6ra1nlYgDCS1FRnbzlw"))
	if err != nil {
		t.Fatal(err)
	}
	want := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	if g := b64.EncodeToString(got); g != want {
		t.Fatalf("got  %s\nwant %s", g, want)
	}
}

func TestSendSignsAndEncrypts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vapid.pem")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	var hdr http.Header
	var body []byte
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr, body = r.Header, mustRead(r.Body)
		if strings.HasSuffix(r.URL.Path, "/gone") {
			w.WriteHeader(http.StatusGone)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	s.Client = srv.Client()
	var sub Subscription
	sub.Endpoint = srv.URL + "/ep"
	sub.Keys.P256dh = b64.EncodeToString(ua.PublicKey().Bytes())
	sub.Keys.Auth = b64.EncodeToString(make([]byte, 16))
	if err := s.Send(context.Background(), sub, Message{Title: "t", Body: "b", URL: "/x", Tag: "thread-1"}, true); err != nil {
		t.Fatal(err)
	}
	if hdr.Get("Content-Encoding") != "aes128gcm" || hdr.Get("Urgency") != "high" || len(body) < 86 {
		t.Fatalf("headers %v, %d bytes", hdr, len(body))
	}
	// The JWT verifies under the key the page subscribes with.
	auth := hdr.Get("Authorization")
	tok := strings.TrimPrefix(strings.Split(auth, ",")[0], "vapid t=")
	parts := strings.Split(tok, ".")
	sig := mustB64(t, parts[2])
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&s.key.PublicKey, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("signature does not verify")
	}
	var claims struct {
		Aud string
		Exp int64
	}
	json.Unmarshal(mustB64(t, parts[1]), &claims)
	if claims.Aud != srv.URL || claims.Exp < time.Now().Unix() {
		t.Fatalf("claims %+v", claims)
	}
	if !strings.Contains(auth, "k="+s.PublicKey()) {
		t.Fatalf("authorization %q", auth)
	}
	sub.Endpoint = srv.URL + "/gone"
	if err := s.Send(context.Background(), sub, Message{}, false); err != ErrGone {
		t.Fatalf("gone: %v", err)
	}
	// The key survives a reload: subscriptions are bound to it.
	again, err := Load(path)
	if err != nil || again.PublicKey() != s.PublicKey() {
		t.Fatalf("reload: %v", err)
	}
}

func mustRead(r io.Reader) []byte {
	b, _ := io.ReadAll(r)
	return b
}
