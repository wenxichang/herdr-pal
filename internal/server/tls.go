package server

import (
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
	"strings"
	"time"
)

const (
	// AutoCertFileName 是自动生成证书的固定文件名。
	AutoCertFileName = "relay-cert.pem"
	// AutoKeyFileName 是自动生成私钥的固定文件名。
	AutoKeyFileName = "relay-key.pem"
)

// TLSConfig 指定外部证书对或自动证书状态目录。
type TLSConfig struct {
	CertFile string
	KeyFile  string
	StateDir string
}

// EnsureTLS 加载外部证书，或生成并持久化可复用的自签名证书。
func EnsureTLS(config TLSConfig) (*tls.Config, error) {
	certConfigured := strings.TrimSpace(config.CertFile) != ""
	keyConfigured := strings.TrimSpace(config.KeyFile) != ""
	if certConfigured != keyConfigured {
		return nil, errors.New("Relay cert_file 与 key_file 必须同时配置")
	}
	if certConfigured {
		return loadTLSCertificate(config.CertFile, config.KeyFile)
	}
	if strings.TrimSpace(config.StateDir) == "" {
		return nil, errors.New("Relay 自动证书状态目录不能为空")
	}
	if err := os.MkdirAll(config.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建 Relay 证书目录: %w", err)
	}
	certPath := filepath.Join(config.StateDir, AutoCertFileName)
	keyPath := filepath.Join(config.StateDir, AutoKeyFileName)
	if fileExists(certPath) && fileExists(keyPath) {
		if err := os.Chmod(keyPath, 0o600); err != nil {
			return nil, fmt.Errorf("收紧 Relay 私钥权限: %w", err)
		}
		return loadTLSCertificate(certPath, keyPath)
	}
	certPEM, keyPEM, err := generateSelfSignedCertificate(time.Now())
	if err != nil {
		return nil, err
	}
	if err := writePrivateFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("写入 Relay 私钥: %w", err)
	}
	if err := writePrivateFileAtomic(certPath, certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("写入 Relay 证书: %w", err)
	}
	return loadTLSCertificate(certPath, keyPath)
}

func loadTLSCertificate(certPath, keyPath string) (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("加载 Relay TLS 证书: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{pair},
	}, nil
}

func generateSelfSignedCertificate(now time.Time) ([]byte, []byte, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("生成 Relay 私钥: %w", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return nil, nil, fmt.Errorf("生成 Relay 证书序号: %w", err)
	}
	hostname, _ := os.Hostname()
	dnsNames := []string{"localhost"}
	if strings.TrimSpace(hostname) != "" && hostname != "localhost" {
		dnsNames = append(dnsNames, hostname)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "herdr-pal-server"},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("生成 Relay 证书: %w", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("编码 Relay 私钥: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	return certPEM, keyPEM, nil
}

func writePrivateFileAtomic(path string, data []byte, mode os.FileMode) (returnErr error) {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".relay-tls-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			if closeErr := temporary.Close(); returnErr == nil && closeErr != nil {
				returnErr = closeErr
			}
		}
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	closed = true
	return os.Rename(temporaryPath, path)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
