package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/config"
)

// writeSelfSignedPair выпускает самоподписанную пару и кладёт её в файлы. Для
// проверки сборки tls.Config настоящий CA не нужен: интересует только то, что
// файлы читаются и складываются в credentials без ошибки.
func writeSelfSignedPair(t *testing.T, dir, name string) (certPath, keyPath string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("генерация ключа: %v", err)
	}

	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		DNSNames:              []string{name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("выпуск сертификата: %v", err)
	}

	certPath = filepath.Join(dir, name+".crt")
	writePEM(t, certPath, &pem.Block{Type: "CERTIFICATE", Bytes: der})

	encoded, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("сериализация ключа: %v", err)
	}

	keyPath = filepath.Join(dir, name+".key")
	writePEM(t, keyPath, &pem.Block{Type: "EC PRIVATE KEY", Bytes: encoded})

	return certPath, keyPath
}

func writePEM(t *testing.T, path string, block *pem.Block) {
	t.Helper()

	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("запись %s: %v", path, err)
	}
}

func tlsConfig(t *testing.T) config.GRPC {
	t.Helper()

	dir := t.TempDir()
	certPath, keyPath := writeSelfSignedPair(t, dir, "server")
	caPath, _ := writeSelfSignedPair(t, dir, "ca")

	return config.GRPC{
		Listen:                ":8443",
		CertFile:              certPath,
		KeyFile:               keyPath,
		ClientCAFile:          caPath,
		CustomerAccessWriters: []string{"product-svc"},
	}
}

func TestTransportCredentialsLoadsPair(t *testing.T) {
	creds, err := transportCredentials(tlsConfig(t))
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if creds == nil {
		t.Fatal("credentials не собраны")
	}
}

// TestTransportCredentialsRejectsGarbageCA — AppendCertsFromPEM молча
// игнорирует мусор и возвращает false. Без явной проверки процесс поднялся бы с
// пустым пулом CA и отверг бы вообще всех, с диагностикой «недостаточно прав»
// вместо «CA не прочитан».
func TestTransportCredentialsRejectsGarbageCA(t *testing.T) {
	cfg := tlsConfig(t)
	garbage := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(garbage, []byte("это не сертификат\n"), 0o600); err != nil {
		t.Fatalf("подготовка файла: %v", err)
	}
	cfg.ClientCAFile = garbage

	_, err := transportCredentials(cfg)
	if err == nil {
		t.Fatal("мусор вместо CA обязан провалить старт")
	}
	if !strings.Contains(err.Error(), "не содержит ни одного сертификата") {
		t.Errorf("ошибка %q не объясняет причину", err)
	}
}

func TestTransportCredentialsRejectsMissingFiles(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "нет-такого")

	tests := []struct {
		name  string
		spoil func(*config.GRPC)
	}{
		{"нет сертификата", func(c *config.GRPC) { c.CertFile = missing }},
		{"нет ключа", func(c *config.GRPC) { c.KeyFile = missing }},
		{"нет CA", func(c *config.GRPC) { c.ClientCAFile = missing }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tlsConfig(t)
			tc.spoil(&cfg)

			if _, err := transportCredentials(cfg); err == nil {
				t.Fatal("отсутствующий файл обязан провалить старт")
			}
		})
	}
}
