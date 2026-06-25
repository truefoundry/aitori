// Package ca manages aitori's per-device certificate authority. The CA is
// generated locally on first run and installed into the OS trust store by the
// platform adapter; its private key never leaves the device (plan §14).
//
// On-the-fly leaf certificates are minted per host (keyed by SNI) so the agent
// can terminate TLS for allowlisted hosts during selective MITM.
package ca

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
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
	"sync"
	"time"
)

const (
	certFile = "ca.pem"
	keyFile  = "ca.key"

	// caValidity is the lifetime of the device CA itself (long-lived).
	caValidity = 10 * 365 * 24 * time.Hour
	// leafValidity is the half-window for minted leaf certs. The total valid
	// window is 2x this (NotBefore = now-leafValidity to tolerate clock skew).
	leafValidity = 7 * 24 * time.Hour
)

var maxSerial = new(big.Int).Lsh(big.NewInt(1), 128)

// CA is a device certificate authority capable of minting leaf certificates.
type CA struct {
	org     string
	cert    *x509.Certificate
	key     *rsa.PrivateKey
	certPEM []byte
	keyID   []byte

	mu    sync.RWMutex
	leafs map[string]*tls.Certificate
}

// Options configures CA creation.
type Options struct {
	Organization string // subject organization, e.g. "aitori"
	CommonName   string // subject CN, e.g. "aitori Device CA"
}

// LoadOrCreate loads the CA from dir, creating (and persisting) a new one if
// none exists. The certificate is written world-readable; the private key is
// written 0600.
func LoadOrCreate(dir string, opts Options) (*CA, error) {
	if opts.Organization == "" {
		opts.Organization = "aitori"
	}
	if opts.CommonName == "" {
		opts.CommonName = "aitori Device CA"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create ca dir: %w", err)
	}

	certPath := filepath.Join(dir, certFile)
	keyPath := filepath.Join(dir, keyFile)

	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if certErr == nil && keyErr == nil {
		ca, err := fromPEM(certPEM, keyPEM, opts.Organization)
		if err == nil {
			return ca, nil
		}
		// Fall through and regenerate on a corrupt/unreadable pair.
	}

	ca, err := newCA(opts)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(certPath, ca.certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("write ca cert: %w", err)
	}
	keyDER := x509.MarshalPKCS1PrivateKey(ca.key)
	keyOut := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyOut, 0o600); err != nil {
		return nil, fmt.Errorf("write ca key: %w", err)
	}
	return ca, nil
}

func newCA(opts Options) (*CA, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	keyID, err := subjectKeyID(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, maxSerial)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: opts.CommonName, Organization: []string{opts.Organization}},
		SubjectKeyId:          keyID,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(caValidity),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{
		org:     opts.Organization,
		cert:    cert,
		key:     key,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyID:   keyID,
		leafs:   make(map[string]*tls.Certificate),
	}, nil
}

func fromPEM(certPEM, keyPEM []byte, org string) (*CA, error) {
	cb, _ := pem.Decode(certPEM)
	if cb == nil || cb.Type != "CERTIFICATE" {
		return nil, errors.New("ca: invalid certificate PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, errors.New("ca: invalid key PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(kb.Bytes)
	if err != nil {
		// Try PKCS#8 as a fallback.
		k, err8 := x509.ParsePKCS8PrivateKey(kb.Bytes)
		if err8 != nil {
			return nil, fmt.Errorf("ca: parse key: %w", err)
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("ca: key is not RSA")
		}
		key = rk
	}
	keyID, err := subjectKeyID(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	return &CA{
		org:     org,
		cert:    cert,
		key:     key,
		certPEM: certPEM,
		keyID:   keyID,
		leafs:   make(map[string]*tls.Certificate),
	}, nil
}

func subjectKeyID(pub *rsa.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum(der)
	return sum[:], nil
}

// CertPath returns the on-disk path of the CA certificate for a given ca_dir.
func CertPath(dir string) string { return filepath.Join(dir, certFile) }

// CertPEM returns the PEM-encoded CA certificate (for trust-store install).
func (c *CA) CertPEM() []byte { return c.certPEM }

// Cert returns the parsed CA certificate.
func (c *CA) Cert() *x509.Certificate { return c.cert }

// LeafForName returns a leaf certificate for host, minting and caching one if
// necessary. host may include a :port, which is stripped.
func (c *CA) LeafForName(host string) (*tls.Certificate, error) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" {
		return nil, errors.New("ca: empty host for leaf certificate")
	}

	c.mu.RLock()
	if tlsc, ok := c.leafs[host]; ok {
		if leafValid(tlsc, host) {
			c.mu.RUnlock()
			return tlsc, nil
		}
	}
	c.mu.RUnlock()

	tlsc, err := c.mintLeaf(host)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.leafs[host] = tlsc
	c.mu.Unlock()
	return tlsc, nil
}

func leafValid(tlsc *tls.Certificate, host string) bool {
	if tlsc.Leaf == nil {
		return false
	}
	now := time.Now()
	if now.Before(tlsc.Leaf.NotBefore) || now.After(tlsc.Leaf.NotAfter) {
		return false
	}
	return tlsc.Leaf.VerifyHostname(host) == nil
}

func (c *CA) mintLeaf(host string) (*tls.Certificate, error) {
	serial, err := rand.Int(rand.Reader, maxSerial)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host, Organization: []string{c.org}},
		SubjectKeyId:          c.keyID,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		NotBefore:             time.Now().Add(-leafValidity),
		NotAfter:              time.Now().Add(leafValidity),
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &c.key.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{
		Certificate: [][]byte{der, c.cert.Raw},
		PrivateKey:  c.key,
		Leaf:        leaf,
	}, nil
}

// TLSConfig returns a *tls.Config that mints certificates on-the-fly from the
// ClientHello SNI. When forceHTTP11 is true, only http/1.1 is advertised on the
// MITM leg (plan §9), which sidesteps HTTP/2 MITM edge cases.
func (c *CA) TLSConfig(forceHTTP11 bool) *tls.Config {
	nextProtos := []string{"h2", "http/1.1"}
	if forceHTTP11 {
		nextProtos = []string{"http/1.1"}
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: nextProtos,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := hello.ServerName
			if name == "" {
				return nil, errors.New("ca: ClientHello has no SNI; cannot mint certificate")
			}
			return c.LeafForName(name)
		},
	}
}
