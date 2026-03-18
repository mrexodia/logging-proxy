package loggingproxy

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

type MITMCAConfig struct {
	CertFile     string
	KeyFile      string
	CommonName   string
	Organization string
	ValidFor     time.Duration
}

func LoadOrCreateMITMCA(config MITMCAConfig) (*tls.Certificate, error) {
	if config.CertFile == "" {
		config.CertFile = filepath.Join("certs", "mitm-ca-cert.pem")
	}
	if config.KeyFile == "" {
		config.KeyFile = filepath.Join("certs", "mitm-ca-key.pem")
	}
	if config.CommonName == "" {
		config.CommonName = "logging-proxy MITM CA"
	}
	if config.Organization == "" {
		config.Organization = "logging-proxy"
	}
	if config.ValidFor == 0 {
		config.ValidFor = 10 * 365 * 24 * time.Hour
	}

	certExists := fileExists(config.CertFile)
	keyExists := fileExists(config.KeyFile)
	if certExists != keyExists {
		return nil, fmt.Errorf("MITM CA files must both exist or both be absent: cert=%s key=%s", config.CertFile, config.KeyFile)
	}

	if !certExists {
		if err := generateMITMCA(config); err != nil {
			return nil, err
		}
	}

	certPEM, err := os.ReadFile(config.CertFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read MITM CA cert %q: %w", config.CertFile, err)
	}
	keyPEM, err := os.ReadFile(config.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read MITM CA key %q: %w", config.KeyFile, err)
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("failed to load MITM CA key pair: %w", err)
	}
	if len(cert.Certificate) == 0 {
		return nil, fmt.Errorf("MITM CA certificate chain is empty")
	}
	cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("failed to parse MITM CA leaf certificate: %w", err)
	}
	if !cert.Leaf.IsCA {
		return nil, fmt.Errorf("MITM certificate %q is not a CA certificate", config.CertFile)
	}

	return &cert, nil
}

func generateMITMCA(config MITMCAConfig) error {
	if err := os.MkdirAll(filepath.Dir(config.CertFile), 0755); err != nil {
		return fmt.Errorf("failed to create MITM cert directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(config.KeyFile), 0755); err != nil {
		return fmt.Errorf("failed to create MITM key directory: %w", err)
	}

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("failed to generate MITM CA private key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("failed to generate MITM CA serial number: %w", err)
	}

	now := time.Now()
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   config.CommonName,
			Organization: []string{config.Organization},
		},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(config.ValidFor),
		BasicConstraintsValid: true,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		MaxPathLenZero:        true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return fmt.Errorf("failed to create MITM CA certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})

	if err := os.WriteFile(config.CertFile, certPEM, 0644); err != nil {
		return fmt.Errorf("failed to write MITM CA certificate %q: %w", config.CertFile, err)
	}
	if err := os.WriteFile(config.KeyFile, keyPEM, 0600); err != nil {
		return fmt.Errorf("failed to write MITM CA private key %q: %w", config.KeyFile, err)
	}

	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
