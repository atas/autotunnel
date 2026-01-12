package httpserver

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"sync"
	"time"
)

// tlsErrorCertProvider generates self-signed certificates for TLS error pages.
// It creates a CA certificate at initialization and generates hostname-specific
// leaf certificates on demand.
type tlsErrorCertProvider struct {
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey

	// Cache for generated certificates (hostname -> cert)
	certCache map[string]*tls.Certificate
	cacheMu   sync.RWMutex
}

// newTLSErrorCertProvider creates a new certificate provider with a fresh CA.
func newTLSErrorCertProvider() (*tlsErrorCertProvider, error) {
	// Generate CA private key (ECDSA P-256 for speed)
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	// Create CA certificate template
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"autotunnel Error CA"},
			CommonName:   "autotunnel Error Certificate Authority",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour), // 1 year
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	// Self-sign the CA certificate
	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}

	caCert, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		return nil, err
	}

	return &tlsErrorCertProvider{
		caCert:    caCert,
		caKey:     caKey,
		certCache: make(map[string]*tls.Certificate),
	}, nil
}

// GetCertificate returns a certificate for the given hostname.
// Certificates are cached and reused for subsequent requests.
func (p *tlsErrorCertProvider) GetCertificate(hostname string) (*tls.Certificate, error) {
	// Check cache first
	p.cacheMu.RLock()
	if cert, ok := p.certCache[hostname]; ok {
		p.cacheMu.RUnlock()
		return cert, nil
	}
	p.cacheMu.RUnlock()

	// Generate new certificate
	cert, err := p.generateCert(hostname)
	if err != nil {
		return nil, err
	}

	// Cache it (with size limit to prevent memory issues)
	p.cacheMu.Lock()
	if len(p.certCache) < 1000 {
		p.certCache[hostname] = cert
	}
	p.cacheMu.Unlock()

	return cert, nil
}

func (p *tlsErrorCertProvider) generateCert(hostname string) (*tls.Certificate, error) {
	// Generate leaf certificate key
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	// Create leaf certificate template
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			Organization: []string{"autotunnel Error Page"},
			CommonName:   hostname,
		},
		DNSNames:    []string{hostname},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().Add(24 * time.Hour), // Short-lived
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	// Sign with CA
	certDER, err := x509.CreateCertificate(rand.Reader, template, p.caCert, &leafKey.PublicKey, p.caKey)
	if err != nil {
		return nil, err
	}

	return &tls.Certificate{
		Certificate: [][]byte{certDER, p.caCert.Raw},
		PrivateKey:  leafKey,
	}, nil
}
