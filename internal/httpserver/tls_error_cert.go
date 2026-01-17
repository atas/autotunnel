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

// tlsErrorCertProvider generates on-the-fly certs for TLS error pages.
// Creates a CA once, then mints short-lived per-hostname certs as needed.
type tlsErrorCertProvider struct {
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey

	certCache map[string]*tls.Certificate
	cacheMu   sync.RWMutex
}

func newTLSErrorCertProvider() (*tlsErrorCertProvider, error) {
	// ECDSA P-256 is fast and widely supported
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

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

func (p *tlsErrorCertProvider) GetCertificate(hostname string) (*tls.Certificate, error) {
	p.cacheMu.RLock()
	if cert, ok := p.certCache[hostname]; ok {
		p.cacheMu.RUnlock()
		return cert, nil
	}
	p.cacheMu.RUnlock()

	cert, err := p.generateCert(hostname)
	if err != nil {
		return nil, err
	}

	// cap cache size to avoid unbounded memory growth
	p.cacheMu.Lock()
	if len(p.certCache) < 1000 {
		p.certCache[hostname] = cert
	}
	p.cacheMu.Unlock()

	return cert, nil
}

func (p *tlsErrorCertProvider) generateCert(hostname string) (*tls.Certificate, error) {
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			Organization: []string{"autotunnel Error Page"},
			CommonName:   hostname,
		},
		DNSNames:    []string{hostname},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().Add(24 * time.Hour), // short-lived, only for error display
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, p.caCert, &leafKey.PublicKey, p.caKey)
	if err != nil {
		return nil, err
	}

	return &tls.Certificate{
		Certificate: [][]byte{certDER, p.caCert.Raw},
		PrivateKey:  leafKey,
	}, nil
}
