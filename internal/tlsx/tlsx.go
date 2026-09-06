// Package tlsx gives starcode HTTPS without a certificate authority on
// the internet: a CA of its own in the data directory, a leaf certificate
// issued from it for this machine's names and addresses, and a listener
// that answers plain HTTP on the same port with a redirect.
//
// Browsers need a secure context for the clipboard, notifications and
// service workers, which is why a console reached over a LAN wants this.
// Installing the CA once on a device (it is served at /starcode-ca.crt)
// makes every certificate the instance issues trusted there.
package tlsx

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	caValidity   = 10 * 365 * 24 * time.Hour
	leafValidity = 397 * 24 * time.Hour // Apple rejects longer server certificates
	renewBefore  = 30 * 24 * time.Hour
	peekTimeout  = 10 * time.Second
)

// Files are where Load keeps the CA and the leaf, under dir.
type Files struct {
	CACert, CAKey, Cert, Key string
}

func files(dir string) Files {
	return Files{
		CACert: filepath.Join(dir, "ca.pem"),
		CAKey:  filepath.Join(dir, "ca-key.pem"),
		Cert:   filepath.Join(dir, "cert.pem"),
		Key:    filepath.Join(dir, "key.pem"),
	}
}

// Load returns a server certificate for hosts (names and IP addresses)
// issued by the CA in dir, creating the CA on first use and re-issuing
// the leaf when it misses a host or is close to expiry. The CA's PEM is
// returned for devices to install.
func Load(dir string, hosts []string) (tls.Certificate, []byte, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, nil, err
	}
	f := files(dir)
	caCert, caKey, caPEM, err := loadOrCreateCA(f)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("certificate authority: %w", err)
	}
	if leaf, err := tls.LoadX509KeyPair(f.Cert, f.Key); err == nil {
		if x, err := x509.ParseCertificate(leaf.Certificate[0]); err == nil && covers(x, hosts) && time.Until(x.NotAfter) > renewBefore && issuedBy(x, caCert) {
			return leaf, caPEM, nil
		}
	}
	leaf, err := issueLeaf(f, caCert, caKey, hosts)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("server certificate: %w", err)
	}
	return leaf, caPEM, nil
}

func loadOrCreateCA(f Files) (*x509.Certificate, *ecdsa.PrivateKey, []byte, error) {
	certPEM, err1 := os.ReadFile(f.CACert)
	keyPEM, err2 := os.ReadFile(f.CAKey)
	if err1 == nil && err2 == nil {
		cert, err := parseCert(certPEM)
		if err != nil {
			return nil, nil, nil, err
		}
		key, err := parseKey(keyPEM)
		if err != nil {
			return nil, nil, nil, err
		}
		if time.Until(cert.NotAfter) > renewBefore {
			return cert, key, certPEM, nil
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	host, _ := os.Hostname()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "starcode CA (" + host + ")", Organization: []string{"starcode"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(caValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, nil, err
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(f.CAKey, keyPEM, 0o600); err != nil {
		return nil, nil, nil, err
	}
	if err := os.WriteFile(f.CACert, certPEM, 0o644); err != nil {
		return nil, nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, err
	}
	// A new CA means the old leaf is no longer trusted through it.
	os.Remove(f.Cert)
	os.Remove(f.Key)
	return cert, key, certPEM, nil
}

func issueLeaf(f Files, caCert *x509.Certificate, caKey *ecdsa.PrivateKey, hosts []string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "starcode", Organization: []string{"starcode"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(f.Key, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(f.Cert, certPEM, 0o644); err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// covers is whether every host is a name or address on the certificate.
func covers(c *x509.Certificate, hosts []string) bool {
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			if !slices.ContainsFunc(c.IPAddresses, func(x net.IP) bool { return x.Equal(ip) }) {
				return false
			}
		} else if !slices.Contains(c.DNSNames, h) {
			return false
		}
	}
	return true
}

func issuedBy(leaf, ca *x509.Certificate) bool {
	return leaf.CheckSignatureFrom(ca) == nil
}

func parseCert(pemBytes []byte) (*x509.Certificate, error) {
	b, _ := pem.Decode(pemBytes)
	if b == nil {
		return nil, errors.New("not a PEM certificate")
	}
	return x509.ParseCertificate(b.Bytes)
}

func parseKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	b, _ := pem.Decode(pemBytes)
	if b == nil {
		return nil, errors.New("not a PEM key")
	}
	return x509.ParseECPrivateKey(b.Bytes)
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

// LocalHosts lists what this machine is called: localhost, its hostname
// (plain and .local), and every non-link-local address on its
// interfaces. extra adds names the machine does not know about itself,
// such as a DNS name on a VPN.
func LocalHosts(extra ...string) []string {
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	if h, err := os.Hostname(); err == nil && h != "" {
		hosts = append(hosts, h)
		if !strings.Contains(h, ".") {
			hosts = append(hosts, h+".local")
		}
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() || ipn.IP.IsLinkLocalMulticast() {
				continue
			}
			hosts = append(hosts, ipn.IP.String())
		}
	}
	for _, e := range extra {
		if e = strings.TrimSpace(e); e != "" {
			hosts = append(hosts, e)
		}
	}
	slices.Sort(hosts)
	return slices.Compact(hosts)
}

// Listen wraps l so each connection is served as TLS when it starts with
// a handshake and as plain HTTP otherwise. The http.Server sees a
// *tls.Conn or a plain conn and sets r.TLS accordingly, which is how the
// handler knows to redirect.
func Listen(l net.Listener, cfg *tls.Config) net.Listener {
	m := &mux{Listener: l, cfg: cfg, conns: make(chan net.Conn), errs: make(chan error, 1)}
	go m.loop()
	return m
}

type mux struct {
	net.Listener
	cfg   *tls.Config
	conns chan net.Conn
	errs  chan error
}

func (m *mux) loop() {
	for {
		c, err := m.Listener.Accept()
		if err != nil {
			m.errs <- err
			return
		}
		// The peek runs off the accept loop so a client that connects and
		// sends nothing cannot hold up the next one.
		go func() {
			br := bufio.NewReader(c)
			c.SetReadDeadline(time.Now().Add(peekTimeout))
			b, err := br.Peek(1)
			c.SetReadDeadline(time.Time{})
			if err != nil {
				c.Close()
				return
			}
			pc := &peeked{Conn: c, r: br}
			if b[0] == 0x16 { // TLS handshake record
				m.conns <- tls.Server(pc, m.cfg)
			} else {
				m.conns <- pc
			}
		}()
	}
}

func (m *mux) Accept() (net.Conn, error) {
	select {
	case c := <-m.conns:
		return c, nil
	case err := <-m.errs:
		// Put it back for the next Accept, as a closed listener keeps
		// failing.
		m.errs <- err
		return nil, err
	}
}

type peeked struct {
	net.Conn
	r *bufio.Reader
}

func (p *peeked) Read(b []byte) (int, error) { return p.r.Read(b) }
