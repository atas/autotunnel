package httpserver

import (
	"testing"
)

func TestNewTLSErrorCertProvider(t *testing.T) {
	provider, err := newTLSErrorCertProvider()
	if err != nil {
		t.Fatalf("Failed to create provider: %v", err)
	}
	if provider.caCert == nil {
		t.Error("CA cert should not be nil")
	}
	if provider.caKey == nil {
		t.Error("CA key should not be nil")
	}
}

func TestGetCertificate_GeneratesValidCert(t *testing.T) {
	provider, _ := newTLSErrorCertProvider()

	cert, err := provider.GetCertificate("test.localhost")
	if err != nil {
		t.Fatalf("Failed to get certificate: %v", err)
	}
	if cert == nil {
		t.Error("Certificate should not be nil")
	}
	if len(cert.Certificate) != 2 { // leaf + CA
		t.Errorf("Expected 2 certs in chain, got %d", len(cert.Certificate))
	}
}

func TestGetCertificate_CacheHit(t *testing.T) {
	provider, _ := newTLSErrorCertProvider()

	cert1, _ := provider.GetCertificate("cached.localhost")
	cert2, _ := provider.GetCertificate("cached.localhost")

	if cert1 != cert2 {
		t.Error("Expected same cert instance from cache")
	}
}

func TestGetCertificate_DifferentHostnames(t *testing.T) {
	provider, _ := newTLSErrorCertProvider()

	cert1, _ := provider.GetCertificate("host1.localhost")
	cert2, _ := provider.GetCertificate("host2.localhost")

	if cert1 == cert2 {
		t.Error("Different hostnames should get different certs")
	}
}
