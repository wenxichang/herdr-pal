package server

import (
	"bytes"
	"crypto/tls"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnsureTLSAutomaticInfoAndPersistentCertificate(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	first, err := EnsureTLS(TLSConfig{StateDir: stateDir})
	if err != nil {
		t.Fatalf("EnsureTLS(first) error = %v", err)
	}
	if first.Config.MinVersion != tls.VersionTLS12 || len(first.Config.Certificates) != 1 {
		t.Fatalf("tls config = %#v", first.Config)
	}
	if first.Info.Mode != TLSModeAutomatic {
		t.Fatalf("TLS mode = %q, want %q", first.Info.Mode, TLSModeAutomatic)
	}
	if !first.Info.NotAfter.After(time.Now()) {
		t.Fatalf("TLS NotAfter = %s", first.Info.NotAfter)
	}
	if len(first.Info.SHA256Fingerprint) != 64 {
		t.Fatalf("TLS fingerprint = %q", first.Info.SHA256Fingerprint)
	}
	if _, err := hex.DecodeString(first.Info.SHA256Fingerprint); err != nil {
		t.Fatalf("TLS fingerprint is not hexadecimal: %v", err)
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
	if !bytes.Equal(first.Config.Certificates[0].Certificate[0], second.Config.Certificates[0].Certificate[0]) {
		t.Fatal("auto-generated certificate was not reused")
	}
	if first.Info != second.Info {
		t.Fatalf("TLS info changed after reload: first=%#v second=%#v", first.Info, second.Info)
	}
}

func TestEnsureTLSExternalInfo(t *testing.T) {
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
	if !bytes.Equal(automatic.Config.Certificates[0].Certificate[0], explicit.Config.Certificates[0].Certificate[0]) {
		t.Fatal("explicit certificate was not loaded")
	}
	if explicit.Info.Mode != TLSModeExternal {
		t.Fatalf("TLS mode = %q, want %q", explicit.Info.Mode, TLSModeExternal)
	}
	if explicit.Info.NotAfter != automatic.Info.NotAfter || explicit.Info.SHA256Fingerprint != automatic.Info.SHA256Fingerprint {
		t.Fatalf("external TLS info = %#v, automatic = %#v", explicit.Info, automatic.Info)
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
