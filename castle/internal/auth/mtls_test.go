package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBuildTLSConfig(t *testing.T) {
	cfg, err := BuildTLSConfig("", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg != nil {
		t.Fatal("expected nil config when tls disabled")
	}

	_, err = BuildTLSConfig("cert.pem", "", "", "")
	if err == nil {
		t.Fatal("expected error when key is missing")
	}
}

func TestBuildTLSConfigWithClientCA(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeSelfSignedCert(t, dir)
	caFile := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caFile, mustRead(certFile), 0o600); err != nil {
		t.Fatal(err)
	}
	clientCA := filepath.Join(dir, "client-ca.pem")
	if err := os.WriteFile(clientCA, mustRead(certFile), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := BuildTLSConfig(certFile, keyFile, caFile, clientCA)
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil {
		t.Fatal("expected tls config")
	}
	if cfg.ClientAuth.String() != "RequireAndVerifyClientCert" {
		t.Fatalf("client auth=%s", cfg.ClientAuth.String())
	}
	if cfg.ClientCAs == nil {
		t.Fatal("expected client CAs")
	}
	if cfg.RootCAs == nil {
		t.Fatal("expected root CAs")
	}
}

func writeSelfSignedCert(t *testing.T, dir string) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func mustRead(path string) []byte {
	b, _ := os.ReadFile(path)
	return b
}
