package loggingproxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreateMITMCARequiresSubjectForGeneration(t *testing.T) {
	logDir := t.TempDir()
	certFile := filepath.Join(logDir, "missing-cert.pem")
	keyFile := filepath.Join(logDir, "missing-key.pem")

	_, err := LoadOrCreateMITMCA(MITMCAConfig{
		CertFile: certFile,
		KeyFile:  keyFile,
	})
	if err == nil {
		t.Fatal("expected missing MITM CA files without generation subject to fail")
	}
	if !strings.Contains(err.Error(), "requires common_name and organization") {
		t.Fatalf("expected missing generation subject error, got %v", err)
	}
	if _, statErr := os.Stat(certFile); !os.IsNotExist(statErr) {
		t.Fatalf("expected cert file not to be created, stat err=%v", statErr)
	}
	if _, statErr := os.Stat(keyFile); !os.IsNotExist(statErr) {
		t.Fatalf("expected key file not to be created, stat err=%v", statErr)
	}
}

func TestLoadOrCreateMITMCALoadsExistingFilesWithoutGenerationSubject(t *testing.T) {
	logDir := t.TempDir()
	certFile := filepath.Join(logDir, "mitm-ca-cert.pem")
	keyFile := filepath.Join(logDir, "mitm-ca-key.pem")

	generated, err := LoadOrCreateMITMCA(MITMCAConfig{
		CertFile:     certFile,
		KeyFile:      keyFile,
		CommonName:   "custom MITM CA",
		Organization: "custom org",
	})
	if err != nil {
		t.Fatalf("failed to generate MITM CA: %v", err)
	}
	if generated.Leaf.Subject.CommonName != "custom MITM CA" {
		t.Fatalf("expected generated common name, got %q", generated.Leaf.Subject.CommonName)
	}

	loaded, err := LoadOrCreateMITMCA(MITMCAConfig{
		CertFile: certFile,
		KeyFile:  keyFile,
	})
	if err != nil {
		t.Fatalf("failed to load existing MITM CA without generation subject: %v", err)
	}
	if loaded.Leaf.Subject.CommonName != "custom MITM CA" {
		t.Fatalf("expected loaded common name, got %q", loaded.Leaf.Subject.CommonName)
	}
}
