package hub

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

func ensureCertificate(stateDir, host string) error {
	certPath := filepath.Join(stateDir, "cert.pem")
	keyPath := filepath.Join(stateDir, "key.pem")
	if fileExists(certPath) && fileExists(keyPath) {
		if err := os.Chmod(keyPath, 0o600); err != nil {
			return fmt.Errorf("secure TLS key: %w", err)
		}
		return nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate TLS key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generate TLS serial: %w", err)
	}
	template := certificateTemplate(serial, host)
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create TLS certificate: %w", err)
	}
	if err := writePEM(certPath, "CERTIFICATE", der, 0o644); err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal TLS key: %w", err)
	}
	return writePEM(keyPath, "EC PRIVATE KEY", keyDER, 0o600)
}

func certificateTemplate(serial *big.Int, host string) *x509.Certificate {
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Device Link Hub"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = append(template.IPAddresses, ip)
	} else if host != "" {
		template.DNSNames = append(template.DNSNames, host)
	}
	return template
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func writePEM(path, kind string, der []byte, mode os.FileMode) error {
	encoded := pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der})
	if err := os.WriteFile(path, encoded, mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}
