package httpserver

import (
	"container/list"
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

const (
	// maxCertCacheSize is the maximum number of certificates to cache
	maxCertCacheSize = 1000
)

// certCacheEntry holds a cached certificate and its key in the LRU list
type certCacheEntry struct {
	hostname string
	cert     *tls.Certificate
}

// tlsErrorCertProvider generates on-the-fly certs for TLS error pages.
// Creates a CA once, then mints short-lived per-hostname certs as needed.
// Uses LRU eviction to bound memory usage while keeping frequently-used certs cached.
type tlsErrorCertProvider struct {
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey

	// LRU cache: map for O(1) lookup, list for access order tracking
	certCache map[string]*list.Element
	lruList   *list.List
	cacheMu   sync.Mutex
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
		certCache: make(map[string]*list.Element),
		lruList:   list.New(),
	}, nil
}

func (p *tlsErrorCertProvider) GetCertificate(hostname string) (*tls.Certificate, error) {
	p.cacheMu.Lock()

	// Check if cert exists in cache
	if elem, ok := p.certCache[hostname]; ok {
		// Move to front (most recently used)
		p.lruList.MoveToFront(elem)
		cert := elem.Value.(*certCacheEntry).cert
		p.cacheMu.Unlock()
		return cert, nil
	}
	p.cacheMu.Unlock()

	// Generate new cert (outside lock to avoid blocking other lookups)
	cert, err := p.generateCert(hostname)
	if err != nil {
		return nil, err
	}

	// Add to cache with LRU eviction
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()

	// Double-check after reacquiring lock (another goroutine may have added it)
	if elem, ok := p.certCache[hostname]; ok {
		p.lruList.MoveToFront(elem)
		return elem.Value.(*certCacheEntry).cert, nil
	}

	// Evict least recently used if at capacity
	if p.lruList.Len() >= maxCertCacheSize {
		oldest := p.lruList.Back()
		if oldest != nil {
			entry := oldest.Value.(*certCacheEntry)
			delete(p.certCache, entry.hostname)
			p.lruList.Remove(oldest)
		}
	}

	// Add new entry at front (most recently used)
	entry := &certCacheEntry{hostname: hostname, cert: cert}
	elem := p.lruList.PushFront(entry)
	p.certCache[hostname] = elem

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
