package server

import (
	"bytes"
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureTLSGeneratesPersistentCertificateAndPrivateKey0600(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	first, err := EnsureTLS(TLSConfig{StateDir: stateDir})
	if err != nil {
		t.Fatalf("EnsureTLS(first) error = %v", err)
	}
	if first.MinVersion != tls.VersionTLS12 || len(first.Certificates) != 1 {
		t.Fatalf("tls config = %#v", first)
	}
	keyPath := filepath.Join(stateDir, AutoKeyFileName)
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode = %o", info.Mode().Perm())
	}

	second, err := EnsureTLS(TLSConfig{StateDir: stateDir})
	if err != nil {
		t.Fatalf("EnsureTLS(second) error = %v", err)
	}
	if !bytes.Equal(first.Certificates[0].Certificate[0], second.Certificates[0].Certificate[0]) {
		t.Fatal("auto-generated certificate was not reused")
	}
}

func TestEnsureTLSLoadsExplicitCertificatePair(t *testing.T) {
	stateDir := t.TempDir()
	automatic, err := EnsureTLS(TLSConfig{StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(stateDir, AutoCertFileName)
	keyPath := filepath.Join(stateDir, AutoKeyFileName)
	explicit, err := EnsureTLS(TLSConfig{CertFile: certPath, KeyFile: keyPath})
	if err != nil {
		t.Fatalf("EnsureTLS(explicit) error = %v", err)
	}
	if !bytes.Equal(automatic.Certificates[0].Certificate[0], explicit.Certificates[0].Certificate[0]) {
		t.Fatal("explicit certificate was not loaded")
	}
}

func TestEnsureTLSRejectsIncompleteCertificatePairAndMissingStateDirectory(t *testing.T) {
	if _, err := EnsureTLS(TLSConfig{CertFile: "cert.pem"}); err == nil {
		t.Fatal("incomplete explicit pair should fail")
	}
	if _, err := EnsureTLS(TLSConfig{}); err == nil {
		t.Fatal("missing state directory should fail")
	}
}
