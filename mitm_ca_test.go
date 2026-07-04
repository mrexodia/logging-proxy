package loggingproxy

import (
	"bytes"
	"crypto/x509"
	"crypto/x509/pkix"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newMITMCATestConfig(certsDir string) MITMCAConfig {
	return MITMCAConfig{
		CertsDir:     certsDir,
		Organization: "Example Inspection",
		CRLHost:      "proxy.example:8080",
	}
}

func TestNewMITMCARequiresCRLHost(t *testing.T) {
	_, err := NewMITMCA(MITMCAConfig{
		CertsDir:     t.TempDir(),
		Organization: "Example Inspection",
	})
	if err == nil {
		t.Fatal("expected missing CRLHost to fail")
	}
	if !strings.Contains(err.Error(), "CRLHost is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewMITMCAGeneratesAndLoadsHierarchy(t *testing.T) {
	certsDir := t.TempDir()
	config := newMITMCATestConfig(certsDir)

	ca, err := NewMITMCA(config)
	if err != nil {
		t.Fatalf("NewMITMCA failed: %v", err)
	}

	for _, name := range []string{"root-ca.crt", "root-ca.key", "intermediate-ca.crt", "intermediate-ca.key"} {
		if _, err := os.Stat(filepath.Join(certsDir, name)); err != nil {
			t.Fatalf("expected %s to exist: %v", name, err)
		}
	}
	if ca.RootCertPath() != filepath.Join(certsDir, "root-ca.crt") {
		t.Fatalf("unexpected root cert path %q", ca.RootCertPath())
	}
	if !ca.rootCert.IsCA {
		t.Fatal("expected root certificate to be a CA")
	}
	if !ca.intermediateCert.IsCA {
		t.Fatal("expected intermediate certificate to be a CA")
	}
	if err := ca.rootCert.CheckSignatureFrom(ca.rootCert); err != nil {
		t.Fatalf("expected root certificate to be self-signed: %v", err)
	}
	if err := ca.intermediateCert.CheckSignatureFrom(ca.rootCert); err != nil {
		t.Fatalf("expected intermediate to be signed by root: %v", err)
	}

	loaded, err := NewMITMCA(config)
	if err != nil {
		t.Fatalf("failed to load existing MITM CA: %v", err)
	}
	if !bytes.Equal(ca.rootCert.Raw, loaded.rootCert.Raw) {
		t.Fatal("expected loaded root certificate to match generated root certificate")
	}
	if !bytes.Equal(ca.intermediateCert.Raw, loaded.intermediateCert.Raw) {
		t.Fatal("expected loaded intermediate certificate to match generated intermediate certificate")
	}
}

func TestNewMITMCARegeneratesExpiringIntermediate(t *testing.T) {
	certsDir := t.TempDir()
	config := newMITMCATestConfig(certsDir)

	ca, err := NewMITMCA(config)
	if err != nil {
		t.Fatalf("NewMITMCA failed: %v", err)
	}

	rootCertPath := filepath.Join(certsDir, "root-ca.crt")
	rootKeyPath := filepath.Join(certsDir, "root-ca.key")
	rootCert, rootKey, err := loadCA(rootCertPath, rootKeyPath)
	if err != nil {
		t.Fatalf("failed to load root CA: %v", err)
	}

	intermediateCertPath := filepath.Join(certsDir, "intermediate-ca.crt")
	intermediateKeyPath := filepath.Join(certsDir, "intermediate-ca.key")
	if err := os.Remove(intermediateCertPath); err != nil {
		t.Fatalf("failed to remove intermediate cert: %v", err)
	}
	if err := os.Remove(intermediateKeyPath); err != nil {
		t.Fatalf("failed to remove intermediate key: %v", err)
	}

	expiring, _, err := loadOrGenerateCA(intermediateCertPath, intermediateKeyPath, pkix.Name{
		Organization: []string{config.Organization},
		CommonName:   config.Organization + " Intermediate CA",
	}, rootCert, rootKey, 24*time.Hour, 0)
	if err != nil {
		t.Fatalf("failed to create expiring intermediate: %v", err)
	}

	renewed, err := NewMITMCA(config)
	if err != nil {
		t.Fatalf("failed to reload MITM CA with expiring intermediate: %v", err)
	}
	if !bytes.Equal(ca.rootCert.Raw, renewed.rootCert.Raw) {
		t.Fatal("expected intermediate renewal to preserve root certificate")
	}
	if bytes.Equal(expiring.Raw, renewed.intermediateCert.Raw) {
		t.Fatal("expected expiring intermediate certificate to be regenerated")
	}
	if !time.Now().Add(caRenewalWindow).Before(renewed.intermediateCert.NotAfter) {
		t.Fatalf("expected renewed intermediate to be valid beyond renewal window, expires at %s", renewed.intermediateCert.NotAfter)
	}
	if err := renewed.intermediateCert.CheckSignatureFrom(renewed.rootCert); err != nil {
		t.Fatalf("expected renewed intermediate to be signed by root: %v", err)
	}
}

func TestNewMITMCARegeneratesIntermediateWhenRootReplaced(t *testing.T) {
	certsDir := t.TempDir()
	config := newMITMCATestConfig(certsDir)

	ca, err := NewMITMCA(config)
	if err != nil {
		t.Fatalf("NewMITMCA failed: %v", err)
	}
	oldIntermediate := append([]byte(nil), ca.intermediateCert.Raw...)

	rootCertPath := filepath.Join(certsDir, "root-ca.crt")
	rootKeyPath := filepath.Join(certsDir, "root-ca.key")
	if err := os.Remove(rootCertPath); err != nil {
		t.Fatalf("failed to remove root cert: %v", err)
	}
	if err := os.Remove(rootKeyPath); err != nil {
		t.Fatalf("failed to remove root key: %v", err)
	}

	replacementRoot, _, err := loadOrGenerateCA(rootCertPath, rootKeyPath, pkix.Name{
		Organization: []string{config.Organization},
		CommonName:   config.Organization + " Replacement Root CA",
	}, nil, nil, 10*365*24*time.Hour, 1)
	if err != nil {
		t.Fatalf("failed to create replacement root: %v", err)
	}

	renewed, err := NewMITMCA(config)
	if err != nil {
		t.Fatalf("failed to reload MITM CA with replaced root: %v", err)
	}
	if !bytes.Equal(replacementRoot.Raw, renewed.rootCert.Raw) {
		t.Fatal("expected replaced root certificate to be loaded")
	}
	if bytes.Equal(oldIntermediate, renewed.intermediateCert.Raw) {
		t.Fatal("expected intermediate certificate to be regenerated after root replacement")
	}
	if err := renewed.intermediateCert.CheckSignatureFrom(renewed.rootCert); err != nil {
		t.Fatalf("expected regenerated intermediate to be signed by replacement root: %v", err)
	}
}

func TestNewMITMCARejectsExpiringRoot(t *testing.T) {
	certsDir := t.TempDir()
	config := newMITMCATestConfig(certsDir)

	_, _, err := loadOrGenerateCA(filepath.Join(certsDir, "root-ca.crt"), filepath.Join(certsDir, "root-ca.key"), pkix.Name{
		Organization: []string{config.Organization},
		CommonName:   config.Organization + " Root CA",
	}, nil, nil, 24*time.Hour, 1)
	if err != nil {
		t.Fatalf("failed to create expiring root: %v", err)
	}

	_, err = NewMITMCA(config)
	if err == nil {
		t.Fatal("expected expiring root CA to fail")
	}
	if !strings.Contains(err.Error(), "manual renewal and re-distribution required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMITMCASignsLeafWithChainCRLAndCache(t *testing.T) {
	ca, err := NewMITMCA(newMITMCATestConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("NewMITMCA failed: %v", err)
	}

	cert, err := ca.getCert("api.example.com:443")
	if err != nil {
		t.Fatalf("failed to sign leaf: %v", err)
	}
	if len(cert.Certificate) != 3 {
		t.Fatalf("expected leaf + intermediate + root chain, got %d certificates", len(cert.Certificate))
	}
	if cert.Leaf == nil {
		t.Fatal("expected parsed leaf certificate")
	}
	if got := cert.Leaf.DNSNames; len(got) != 1 || got[0] != "api.example.com" {
		t.Fatalf("unexpected leaf DNS names: %#v", got)
	}
	if got := cert.Leaf.CRLDistributionPoints; len(got) != 1 || got[0] != "http://proxy.example:8080/crl" {
		t.Fatalf("unexpected CRL distribution points: %#v", got)
	}
	if err := cert.Leaf.CheckSignatureFrom(ca.intermediateCert); err != nil {
		t.Fatalf("expected leaf to be signed by intermediate: %v", err)
	}

	roots := x509.NewCertPool()
	roots.AddCert(ca.rootCert)
	intermediates := x509.NewCertPool()
	intermediates.AddCert(ca.intermediateCert)
	if _, err := cert.Leaf.Verify(x509.VerifyOptions{
		DNSName:       "api.example.com",
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("failed to verify leaf chain: %v", err)
	}

	cached, err := ca.getCert("api.example.com:443")
	if err != nil {
		t.Fatalf("failed to get cached leaf: %v", err)
	}
	if !bytes.Equal(cert.Certificate[0], cached.Certificate[0]) {
		t.Fatal("expected cached leaf certificate for repeated host")
	}
}

func TestMITMCASignsIPAddressLeaf(t *testing.T) {
	ca, err := NewMITMCA(newMITMCATestConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("NewMITMCA failed: %v", err)
	}

	cert, err := ca.getCert("127.0.0.1:443")
	if err != nil {
		t.Fatalf("failed to sign IP leaf: %v", err)
	}
	if len(cert.Leaf.IPAddresses) != 1 || !cert.Leaf.IPAddresses[0].Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("unexpected leaf IP addresses: %#v", cert.Leaf.IPAddresses)
	}
}

func TestMITMCAServeCRL(t *testing.T) {
	ca, err := NewMITMCA(newMITMCATestConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("NewMITMCA failed: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/crl", nil)
	response := httptest.NewRecorder()
	ca.ServeCRL(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected CRL status 200, got %d", response.Code)
	}
	if got := response.Header().Get("Content-Type"); got != "application/pkix-crl" {
		t.Fatalf("unexpected CRL content type %q", got)
	}
	crl, err := x509.ParseRevocationList(response.Body.Bytes())
	if err != nil {
		t.Fatalf("failed to parse CRL: %v", err)
	}
	if crl.Number.Int64() != 1 {
		t.Fatalf("expected initial CRL number 1, got %s", crl.Number)
	}
	if err := crl.CheckSignatureFrom(ca.intermediateCert); err != nil {
		t.Fatalf("expected CRL to be signed by intermediate: %v", err)
	}
}
