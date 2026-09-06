package tlsx

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadIssuesAndReissues(t *testing.T) {
	dir := t.TempDir()
	cert, caPEM, err := Load(dir, []string{"localhost", "127.0.0.1", "box.local"})
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("CA PEM did not parse")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"localhost", "box.local"} {
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: name, Roots: pool}); err != nil {
			t.Errorf("verify %s: %v", name, err)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "127.0.0.1", Roots: pool}); err != nil {
		t.Errorf("verify ip: %v", err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "ca-key.pem")); fi.Mode().Perm() != 0o600 {
		t.Errorf("ca key mode = %v", fi.Mode().Perm())
	}

	// Same hosts: the leaf is reused. A new host: re-issued under the same CA.
	again, caAgain, err := Load(dir, []string{"localhost"})
	if err != nil || string(caAgain) != string(caPEM) || string(again.Certificate[0]) != string(cert.Certificate[0]) {
		t.Fatalf("second load changed the certificate (err %v)", err)
	}
	more, caMore, err := Load(dir, []string{"localhost", "10.0.0.5"})
	if err != nil || string(caMore) != string(caPEM) || string(more.Certificate[0]) == string(cert.Certificate[0]) {
		t.Fatalf("third load did not re-issue (err %v)", err)
	}
	leaf, _ = x509.ParseCertificate(more.Certificate[0])
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "10.0.0.5", Roots: pool}); err != nil {
		t.Errorf("verify new ip: %v", err)
	}
}

func TestListenServesBothSchemes(t *testing.T) {
	cert, caPEM, err := Load(t.TempDir(), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", "http/1.1"}}
	srv := &http.Server{TLSConfig: cfg, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			http.Redirect(w, r, "https://"+r.Host+r.URL.RequestURI(), http.StatusTemporaryRedirect)
			return
		}
		io.WriteString(w, "secure "+r.Proto)
	})}
	go srv.Serve(Listen(raw, cfg))
	defer srv.Close()
	addr := raw.Addr().String()

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}, ForceAttemptHTTP2: true}, Timeout: 5 * time.Second}
	resp, err := client.Get("https://" + addr + "/x")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.HasPrefix(string(body), "secure HTTP/2") {
		t.Errorf("https body = %q", body)
	}

	plain := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Timeout: 5 * time.Second}
	resp, err = plain.Get("http://" + addr + "/x?y=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || resp.Header.Get("Location") != "https://"+addr+"/x?y=1" {
		t.Errorf("http redirect = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}
