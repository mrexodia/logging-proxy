package loggingproxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/elazarl/goproxy"
)

// MITMCAConfig configures the self-contained MITM certificate authority.
type MITMCAConfig struct {
	// CertsDir is the directory for persistent CA files (root-ca.crt, root-ca.key,
	// intermediate-ca.crt, intermediate-ca.key). On first run these are generated
	// automatically; on subsequent runs they are loaded from disk.
	// Distribute {CertsDir}/root-ca.crt to client machines and trust it.
	CertsDir string

	// Organization appears in the Subject of all generated certificates.
	Organization string

	// CRLHost is the host:port where the proxy is reachable, used to build the
	// CRL distribution point URL embedded in leaf certificates.
	CRLHost string
}

// MITMCA is a self-contained certificate authority for TLS interception.
// It manages a root CA, intermediate CA, leaf certificate generation with
// caching, and a CRL endpoint.
type MITMCA struct {
	rootCert         *x509.Certificate
	intermediateCert *x509.Certificate
	intermediateKey  *ecdsa.PrivateKey
	chain            [][]byte // [intermediate DER, root DER]
	crlURL           string
	org              string
	certsDir         string

	certsMu sync.Mutex
	certs   map[string]*tls.Certificate

	crlMu  sync.RWMutex
	crl    []byte
	crlNum int64
}

// NewMITMCA loads or generates the CA hierarchy and starts CRL refresh.
func NewMITMCA(config MITMCAConfig) (*MITMCA, error) {
	if config.CertsDir == "" {
		config.CertsDir = "certs"
	}
	if config.Organization == "" {
		config.Organization = "Network Inspection"
	}
	if config.CRLHost == "" {
		return nil, fmt.Errorf("MITMCAConfig.CRLHost is required")
	}

	if err := os.MkdirAll(config.CertsDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create certs directory: %w", err)
	}

	rootCertPath := filepath.Join(config.CertsDir, "root-ca.crt")
	rootKeyPath := filepath.Join(config.CertsDir, "root-ca.key")
	intCertPath := filepath.Join(config.CertsDir, "intermediate-ca.crt")
	intKeyPath := filepath.Join(config.CertsDir, "intermediate-ca.key")

	rootCert, rootKey, err := loadOrGenerateCA(rootCertPath, rootKeyPath, pkix.Name{
		Organization: []string{config.Organization},
		CommonName:   config.Organization + " Root CA",
	}, nil, nil, 10*365*24*time.Hour, 1)
	if err != nil {
		return nil, fmt.Errorf("root CA: %w", err)
	}

	intCert, intKey, err := loadOrGenerateCA(intCertPath, intKeyPath, pkix.Name{
		Organization: []string{config.Organization},
		CommonName:   config.Organization + " Intermediate CA",
	}, rootCert, rootKey, 5*365*24*time.Hour, 0)
	if err != nil {
		return nil, fmt.Errorf("intermediate CA: %w", err)
	}

	ca := &MITMCA{
		rootCert:         rootCert,
		intermediateCert: intCert,
		intermediateKey:  intKey,
		chain:            [][]byte{intCert.Raw, rootCert.Raw},
		crlURL:           fmt.Sprintf("http://%s/crl", config.CRLHost),
		org:              config.Organization,
		certsDir:         config.CertsDir,
		certs:            make(map[string]*tls.Certificate),
		crlNum:           1,
	}

	if err := ca.refreshCRL(); err != nil {
		return nil, fmt.Errorf("failed to generate initial CRL: %w", err)
	}

	go ca.crlRefreshLoop()

	return ca, nil
}

// RootCertPath returns the path to the root CA certificate file.
// Distribute this file to client machines and add it to their trust stores.
func (ca *MITMCA) RootCertPath() string {
	return filepath.Join(ca.certsDir, "root-ca.crt")
}

// TLSConfigForHost returns a function compatible with goproxy's ConnectAction.TLSConfig.
func (ca *MITMCA) TLSConfigForHost() func(string, *goproxy.ProxyCtx) (*tls.Config, error) {
	return func(host string, ctx *goproxy.ProxyCtx) (*tls.Config, error) {
		cert, err := ca.getCert(host)
		if err != nil {
			return nil, err
		}
		return &tls.Config{Certificates: []tls.Certificate{*cert}}, nil
	}
}

// ServeCRL handles HTTP requests for the CRL distribution point.
// Wire this to the "/crl" path on your proxy's HTTP handler.
func (ca *MITMCA) ServeCRL(w http.ResponseWriter, r *http.Request) {
	ca.crlMu.RLock()
	crl := ca.crl
	ca.crlMu.RUnlock()

	w.Header().Set("Content-Type", "application/pkix-crl")
	w.Write(crl)
}

func (ca *MITMCA) getCert(host string) (*tls.Certificate, error) {
	hostname := stripPort(host)

	ca.certsMu.Lock()
	defer ca.certsMu.Unlock()

	if cert, ok := ca.certs[hostname]; ok {
		if cert.Leaf != nil && time.Now().Before(cert.Leaf.NotAfter.Add(-5*time.Minute)) {
			return cert, nil
		}
		delete(ca.certs, hostname)
	}

	cert, err := ca.signHost(hostname)
	if err != nil {
		return nil, err
	}
	ca.certs[hostname] = cert
	return cert, nil
}

func (ca *MITMCA) signHost(hostname string) (*tls.Certificate, error) {
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("failed to generate serial number: %w", err)
	}

	now := time.Now()
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{ca.org},
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		CRLDistributionPoints: []string{ca.crlURL},
	}

	if ip := net.ParseIP(hostname); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{hostname}
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate leaf key: %w", err)
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, ca.intermediateCert, &leafKey.PublicKey, ca.intermediateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to sign certificate for %s: %w", hostname, err)
	}

	// Full chain: leaf + intermediate + root
	chain := [][]byte{certDER}
	chain = append(chain, ca.chain...)

	leaf, _ := x509.ParseCertificate(certDER)

	return &tls.Certificate{
		Certificate: chain,
		PrivateKey:  leafKey,
		Leaf:        leaf,
	}, nil
}

func (ca *MITMCA) refreshCRL() error {
	now := time.Now()
	template := &x509.RevocationList{
		Number:     big.NewInt(ca.crlNum),
		ThisUpdate: now,
		NextUpdate: now.Add(7 * 24 * time.Hour),
	}

	crl, err := x509.CreateRevocationList(rand.Reader, template, ca.intermediateCert, ca.intermediateKey)
	if err != nil {
		return err
	}

	ca.crlMu.Lock()
	ca.crl = crl
	ca.crlNum++
	ca.crlMu.Unlock()

	return nil
}

func (ca *MITMCA) crlRefreshLoop() {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		_ = ca.refreshCRL()
	}
}

const caRenewalWindow = 30 * 24 * time.Hour

// loadOrGenerateCA loads a CA cert+key from disk, or generates and persists a new one.
// If parent is nil, the CA is self-signed (root).
//
// Existing intermediate certificates are regenerated when they expire within
// caRenewalWindow or were not signed by the current parent. Existing root
// certificates cannot be auto-regenerated because clients must re-trust them.
func loadOrGenerateCA(certPath, keyPath string, subject pkix.Name, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, validity time.Duration, pathLen int) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certExists := fileExists(certPath)
	keyExists := fileExists(keyPath)
	if certExists != keyExists {
		return nil, nil, fmt.Errorf("CA files must both exist or both be absent: cert=%s key=%s", certPath, keyPath)
	}
	if certExists {
		cert, key, err := loadCA(certPath, keyPath)
		if err != nil {
			return nil, nil, err
		}

		needsRegen := false
		if time.Now().Add(caRenewalWindow).After(cert.NotAfter) {
			if parent == nil {
				return nil, nil, fmt.Errorf("root CA at %s expires %s, manual renewal and re-distribution required", certPath, cert.NotAfter.Format(time.DateOnly))
			}
			needsRegen = true
		}
		if parent != nil && cert.CheckSignatureFrom(parent) != nil {
			needsRegen = true
		}
		if !needsRegen {
			return cert, key, nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(certPath), 0755); err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0755); err != nil {
		return nil, nil, err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate serial: %w", err)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               subject,
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(validity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		MaxPathLen:            pathLen,
		MaxPathLenZero:        pathLen == 0,
	}

	issuer := template
	signingKey := key
	if parent != nil {
		issuer = parent
		signingKey = parentKey
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, issuer, &key.PublicKey, signingKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		return nil, nil, fmt.Errorf("failed to write cert: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return nil, nil, fmt.Errorf("failed to write key: %w", err)
	}

	cert, _ := x509.ParseCertificate(certDER)
	return cert, key, nil
}

func loadCA(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read cert %s: %w", certPath, err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read key %s: %w", keyPath, err)
	}

	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, nil, fmt.Errorf("failed to decode certificate PEM from %s", certPath)
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse certificate from %s: %w", certPath, err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("failed to decode key PEM from %s", keyPath)
	}

	// Try EC private key format first, fall back to PKCS8
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		rawKey, err2 := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
		if err2 != nil {
			return nil, nil, fmt.Errorf("failed to parse key from %s: %w (also tried PKCS8: %w)", keyPath, err, err2)
		}
		var ok bool
		key, ok = rawKey.(*ecdsa.PrivateKey)
		if !ok {
			return nil, nil, fmt.Errorf("key in %s is not ECDSA", keyPath)
		}
	}

	return cert, key, nil
}

func stripPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
