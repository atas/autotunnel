package httpserver

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

func (s *Server) handleTLSConnection(conn *peekConn) {
	defer conn.Close()

	// Set deadline for reading ClientHello
	_ = conn.Conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	// Read enough for ClientHello
	buf := make([]byte, 16384)
	n, err := conn.Read(buf)
	if err != nil {
		if s.config.Verbose {
			log.Printf("[tls] Error reading ClientHello: %v", err)
		}
		return
	}

	// Clear deadline
	_ = conn.Conn.SetReadDeadline(time.Time{})

	// Extract SNI
	sni, err := extractSNI(buf[:n])
	if err != nil {
		log.Printf("[tls] Failed to extract SNI: %v", err)
		s.sendTLSErrorPage(conn.Conn, buf[:n], "", tlsErrorSNIExtraction, fmt.Sprintf("Failed to extract SNI: %v", err))
		return
	}

	if s.config.Verbose {
		log.Printf("[tls] [%s] New connection", sni)
	}

	// Look up or create tunnel
	tunnel, err := s.manager.GetOrCreateTunnel(sni, "https")
	if err != nil {
		log.Printf("[tls] [%s] Error: %v", sni, err)
		s.sendTLSErrorPage(conn.Conn, buf[:n], sni, tlsErrorRouteNotFound, fmt.Sprintf("No service configured for host: %s", sni))
		return
	}

	// Ensure tunnel is running
	if !tunnel.IsRunning() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := tunnel.Start(ctx); err != nil {
			cancel()
			log.Printf("[tls] [%s] Failed to start tunnel: %v", sni, err)
			s.sendTLSErrorPage(conn.Conn, buf[:n], sni, tlsErrorTunnelStartup, fmt.Sprintf("Failed to start tunnel: %v", err))
			return
		}
		cancel()
	}

	tunnel.Touch()

	// Connect to backend
	backendAddr := fmt.Sprintf("127.0.0.1:%d", tunnel.LocalPort())
	backendConn, err := net.DialTimeout("tcp", backendAddr, 10*time.Second)
	if err != nil {
		log.Printf("[tls] [%s] Failed to connect to backend: %v", sni, err)
		s.sendTLSErrorPage(conn.Conn, buf[:n], sni, tlsErrorBackendConnection, fmt.Sprintf("Failed to connect to backend: %v", err))
		return
	}
	defer backendConn.Close()

	// Forward the ClientHello
	if _, err := backendConn.Write(buf[:n]); err != nil {
		log.Printf("[tls] [%s] Failed to forward ClientHello: %v", sni, err)
		s.sendTLSErrorPage(conn.Conn, buf[:n], sni, tlsErrorForwarding, fmt.Sprintf("Failed to forward ClientHello: %v", err))
		return
	}

	// Bidirectional copy
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, _ = io.Copy(backendConn, conn.Conn)
		if tc, ok := backendConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()
		_, _ = io.Copy(conn.Conn, backendConn)
		if tc, ok := conn.Conn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()

	wg.Wait()
}

// extractSNI parses the TLS ClientHello and extracts the SNI extension
func extractSNI(data []byte) (string, error) {
	// TLS record header: type(1) + version(2) + length(2)
	if len(data) < 5 {
		return "", fmt.Errorf("data too short for TLS record")
	}

	// Check if it's a TLS handshake (0x16)
	if data[0] != 0x16 {
		return "", fmt.Errorf("not a TLS handshake record")
	}

	// Record length
	recordLen := int(data[3])<<8 | int(data[4])
	if len(data) < 5+recordLen {
		return "", fmt.Errorf("incomplete TLS record")
	}

	// Handshake header: type(1) + length(3)
	handshake := data[5:]
	if len(handshake) < 4 {
		return "", fmt.Errorf("data too short for handshake header")
	}

	// Check if it's a ClientHello (0x01)
	if handshake[0] != 0x01 {
		return "", fmt.Errorf("not a ClientHello")
	}

	// Skip handshake header (4 bytes) + version (2) + random (32) = 38 bytes
	pos := 38
	if len(handshake) < pos+1 {
		return "", fmt.Errorf("data too short for session ID length")
	}

	// Session ID
	sessionIDLen := int(handshake[pos])
	pos += 1 + sessionIDLen
	if len(handshake) < pos+2 {
		return "", fmt.Errorf("data too short for cipher suites length")
	}

	// Cipher suites
	cipherSuitesLen := int(handshake[pos])<<8 | int(handshake[pos+1])
	pos += 2 + cipherSuitesLen
	if len(handshake) < pos+1 {
		return "", fmt.Errorf("data too short for compression methods length")
	}

	// Compression methods
	compressionLen := int(handshake[pos])
	pos += 1 + compressionLen
	if len(handshake) < pos+2 {
		return "", fmt.Errorf("no extensions present")
	}

	// Extensions length
	extensionsLen := int(handshake[pos])<<8 | int(handshake[pos+1])
	pos += 2
	extensionsEnd := pos + extensionsLen

	// Parse extensions
	for pos+4 <= extensionsEnd && pos+4 <= len(handshake) {
		extType := int(handshake[pos])<<8 | int(handshake[pos+1])
		extLen := int(handshake[pos+2])<<8 | int(handshake[pos+3])
		pos += 4

		if pos+extLen > len(handshake) {
			break
		}

		// SNI extension type is 0x0000
		if extType == 0 {
			// SNI extension data: list length (2) + type (1) + name length (2) + name
			extData := handshake[pos : pos+extLen]
			if len(extData) < 5 {
				return "", fmt.Errorf("SNI extension too short")
			}
			// Skip list length (2) + type (1)
			nameLen := int(extData[3])<<8 | int(extData[4])
			if len(extData) < 5+nameLen {
				return "", fmt.Errorf("SNI name truncated")
			}
			return string(extData[5 : 5+nameLen]), nil
		}

		pos += extLen
	}

	return "", fmt.Errorf("SNI extension not found")
}
