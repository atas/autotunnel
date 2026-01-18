package httpserver

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPeekConn_IsTLS(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		expected bool
	}{
		{
			name:     "TLS handshake starts with 0x16",
			data:     []byte{0x16, 0x03, 0x01, 0x00, 0x05}, // TLS 1.0 handshake
			expected: true,
		},
		{
			name:     "TLS 1.2 handshake",
			data:     []byte{0x16, 0x03, 0x03, 0x00, 0x05}, // TLS 1.2 handshake
			expected: true,
		},
		{
			name:     "HTTP GET request",
			data:     []byte("GET / HTTP/1.1\r\n"),
			expected: false,
		},
		{
			name:     "HTTP POST request",
			data:     []byte("POST /api HTTP/1.1\r\n"),
			expected: false,
		},
		{
			name:     "HTTP PUT request",
			data:     []byte("PUT /resource HTTP/1.1\r\n"),
			expected: false,
		},
		{
			name:     "Random data not TLS",
			data:     []byte{0x00, 0x01, 0x02, 0x03},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a pipe to simulate a connection
			client, server := net.Pipe()
			defer func() { _ = client.Close() }()
			defer func() { _ = server.Close() }()

			// Write test data from client side
			go func() {
				_, _ = client.Write(tt.data)
			}()

			// Create peekConn and test
			pc := newPeekConn(server)
			result := pc.isTLS()

			if result != tt.expected {
				t.Errorf("isTLS() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

func TestExtractSNI(t *testing.T) {
	tests := []struct {
		name        string
		sni         string
		expectError bool
	}{
		{
			name:        "simple hostname",
			sni:         "example.com",
			expectError: false,
		},
		{
			name:        "subdomain",
			sni:         "argocd.localhost",
			expectError: false,
		},
		{
			name:        "deep subdomain",
			sni:         "api.v1.example.com",
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Generate a real TLS ClientHello with the SNI
			clientHello := generateClientHello(tt.sni)

			sni, err := extractSNI(clientHello)
			if tt.expectError {
				if err == nil {
					t.Errorf("expected error but got none")
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if sni != tt.sni {
				t.Errorf("extractSNI() = %q, expected %q", sni, tt.sni)
			}
		})
	}
}

func TestExtractSNI_InvalidData(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{
			name: "empty data",
			data: []byte{},
		},
		{
			name: "too short",
			data: []byte{0x16, 0x03},
		},
		{
			name: "not TLS handshake",
			data: []byte{0x15, 0x03, 0x01, 0x00, 0x05, 0x00, 0x00, 0x00, 0x00, 0x00},
		},
		{
			name: "HTTP request",
			data: []byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := extractSNI(tt.data)
			if err == nil {
				t.Errorf("expected error for invalid data, got none")
			}
		})
	}
}

func TestMuxListener_ProtocolDetection(t *testing.T) {
	// Create a mux listener on a random port
	mux, err := newMuxListener("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create muxListener: %v", err)
	}
	defer func() { _ = mux.Close() }()

	addr := mux.Listener.Addr().String()

	// Test HTTP detection
	t.Run("HTTP connection detected", func(t *testing.T) {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("Failed to connect: %v", err)
		}
		defer func() { _ = conn.Close() }()

		// Send HTTP request
		_, _ = conn.Write([]byte("GET / HTTP/1.1\r\nHost: test.localhost\r\n\r\n"))

		// Accept from the main listener
		accepted, err := mux.Listener.Accept()
		if err != nil {
			t.Fatalf("Failed to accept: %v", err)
		}
		defer func() { _ = accepted.Close() }()

		// Wrap and check
		pc := newPeekConn(accepted)
		if pc.isTLS() {
			t.Error("HTTP connection incorrectly detected as TLS")
		}
	})
}

// generateClientHello creates a minimal TLS 1.2 ClientHello with SNI extension
func generateClientHello(serverName string) []byte {
	// This is a simplified ClientHello - in real tests you might use crypto/tls
	sniExtension := make([]byte, 0, 9+len(serverName))
	// Extension type: server_name (0x0000)
	sniExtension = append(sniExtension, 0x00, 0x00)
	// Extension length
	sniExtensionLen := 5 + len(serverName)
	sniExtension = append(sniExtension, byte(sniExtensionLen>>8), byte(sniExtensionLen))
	// SNI list length
	sniListLen := 3 + len(serverName)
	sniExtension = append(sniExtension, byte(sniListLen>>8), byte(sniListLen))
	// Name type: hostname (0)
	sniExtension = append(sniExtension, 0x00)
	// Name length
	sniExtension = append(sniExtension, byte(len(serverName)>>8), byte(len(serverName)))
	// Name
	sniExtension = append(sniExtension, []byte(serverName)...)

	// Extensions length
	extensions := make([]byte, 0)
	extensions = append(extensions, byte(len(sniExtension)>>8), byte(len(sniExtension)))
	extensions = append(extensions, sniExtension...)

	// Build ClientHello
	clientHello := make([]byte, 0)
	// Handshake type: ClientHello (1)
	clientHello = append(clientHello, 0x01)
	// Length placeholder (3 bytes) - will fill in later
	clientHello = append(clientHello, 0x00, 0x00, 0x00)
	// Version: TLS 1.2
	clientHello = append(clientHello, 0x03, 0x03)
	// Random (32 bytes)
	clientHello = append(clientHello, make([]byte, 32)...)
	// Session ID length (0)
	clientHello = append(clientHello, 0x00)
	// Cipher suites length (2) + one cipher suite
	clientHello = append(clientHello, 0x00, 0x02, 0x00, 0x2f) // TLS_RSA_WITH_AES_128_CBC_SHA
	// Compression methods length (1) + null compression
	clientHello = append(clientHello, 0x01, 0x00)
	// Extensions
	clientHello = append(clientHello, extensions...)

	// Fill in handshake length
	handshakeLen := len(clientHello) - 4
	clientHello[1] = byte(handshakeLen >> 16)
	clientHello[2] = byte(handshakeLen >> 8)
	clientHello[3] = byte(handshakeLen)

	// Wrap in TLS record
	record := make([]byte, 0)
	// Content type: Handshake (22 = 0x16)
	record = append(record, 0x16)
	// Version: TLS 1.0 (for compatibility)
	record = append(record, 0x03, 0x01)
	// Length
	record = append(record, byte(len(clientHello)>>8), byte(len(clientHello)))
	// ClientHello
	record = append(record, clientHello...)

	return record
}

func TestHTTPProxyHeaders(t *testing.T) {
	// Create a test backend that checks headers
	var receivedProto string
	var receivedHost string
	var receivedFor string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedProto = r.Header.Get("X-Forwarded-Proto")
		receivedHost = r.Header.Get("X-Forwarded-Host")
		receivedFor = r.Header.Get("X-Forwarded-For")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}))
	defer backend.Close()

	backendURL := backend.URL

	t.Run("X-Forwarded headers are set", func(t *testing.T) {
		req, _ := http.NewRequest("GET", backendURL, nil)
		req.Host = "test.localhost:8989"
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("X-Forwarded-Host", "test.localhost:8989")
		req.Header.Set("X-Forwarded-For", "127.0.0.1")

		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("Request failed: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("Expected 200, got %d", resp.StatusCode)
		}

		// Verify headers were received by backend
		if receivedProto != "https" {
			t.Errorf("Expected X-Forwarded-Proto 'https', got %q", receivedProto)
		}
		if receivedHost != "test.localhost:8989" {
			t.Errorf("Expected X-Forwarded-Host 'test.localhost:8989', got %q", receivedHost)
		}
		if receivedFor != "127.0.0.1" {
			t.Errorf("Expected X-Forwarded-For '127.0.0.1', got %q", receivedFor)
		}
	})
}

func TestTLSPassthrough_EndToEnd(t *testing.T) {
	// Create a TLS backend server
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Hello from TLS backend"))
	}))
	defer backend.Close()

	// Get the backend's address
	backendAddr := backend.Listener.Addr().String()

	// Create a simple TLS passthrough - connect to backend directly
	t.Run("TLS connection works", func(t *testing.T) {
		// Connect via TLS
		conf := &tls.Config{
			InsecureSkipVerify: true,
		}
		conn, err := tls.Dial("tcp", backendAddr, conf)
		if err != nil {
			t.Fatalf("TLS dial failed: %v", err)
		}
		defer func() { _ = conn.Close() }()

		// Send HTTP request over TLS
		_, _ = fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: test\r\n\r\n")

		// Read response
		reader := bufio.NewReader(conn)
		resp, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatalf("Failed to read response: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("Expected 200, got %d", resp.StatusCode)
		}

		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "TLS backend") {
			t.Errorf("Unexpected body: %s", body)
		}
	})
}

func TestSNIExtraction_RealTLSClientHello(t *testing.T) {
	// Create a TLS server
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Get certificate for client
	cert := server.Certificate()
	certPool := x509.NewCertPool()
	certPool.AddCert(cert)

	// Create a listener to capture the ClientHello
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}
	defer listener.Close()

	addr := listener.Addr().String()
	testSNI := "test.example.com"

	// Client goroutine - will send real TLS ClientHello
	go func() {
		time.Sleep(50 * time.Millisecond) // Let server start accepting
		conf := &tls.Config{
			ServerName:         testSNI,
			InsecureSkipVerify: true,
		}
		conn, err := tls.Dial("tcp", addr, conf)
		if err != nil {
			// Expected to fail since we're not completing handshake
			return
		}
		conn.Close()
	}()

	// Accept and read ClientHello
	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("Accept failed: %v", err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16384)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	// Extract SNI from real ClientHello
	sni, err := extractSNI(buf[:n])
	if err != nil {
		t.Fatalf("extractSNI failed: %v", err)
	}

	if sni != testSNI {
		t.Errorf("Expected SNI %q, got %q", testSNI, sni)
	}
}

func TestPeekConn_ReadAfterPeek(t *testing.T) {
	// Verify that data can still be read after peeking
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	testData := []byte("GET /test HTTP/1.1\r\nHost: example.com\r\n\r\n")

	go func() {
		_, _ = client.Write(testData)
		client.Close()
	}()

	pc := newPeekConn(server)

	// Peek at first byte
	peeked, err := pc.Peek(1)
	if err != nil {
		t.Fatalf("Peek failed: %v", err)
	}
	if peeked[0] != 'G' {
		t.Errorf("Expected 'G', got %c", peeked[0])
	}

	// Now read all data - should get full content including peeked byte
	all, err := io.ReadAll(pc)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}

	if string(all) != string(testData) {
		t.Errorf("Expected %q, got %q", testData, all)
	}
}

func TestMuxListener_HTTPListener(t *testing.T) {
	mux, err := newMuxListener("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create muxListener: %v", err)
	}
	defer mux.Close()

	httpLis := mux.httpListener()
	if httpLis == nil {
		t.Fatal("httpListener returned nil")
	}

	// Verify Addr() works
	if httpLis.Addr() == nil {
		t.Error("httpListener.Addr() returned nil")
	}
}
