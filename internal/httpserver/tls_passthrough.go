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

	// give slow clients 10s to send ClientHello
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

	_ = conn.Conn.SetReadDeadline(time.Time{})

	sni, err := extractSNI(buf[:n])
	if err != nil {
		log.Printf("[tls] Failed to extract SNI: %v", err)
		s.sendTLSErrorPage(conn.Conn, buf[:n], "", tlsErrorSNIExtraction, fmt.Sprintf("Failed to extract SNI: %v", err))
		return
	}

	if s.config.Verbose {
		log.Printf("[tls] [%s] New connection", sni)
	}

	tunnel, err := s.manager.GetOrCreateTunnel(sni, "https")
	if err != nil {
		log.Printf("[tls] [%s] Error: %v", sni, err)
		s.sendTLSErrorPage(conn.Conn, buf[:n], sni, tlsErrorRouteNotFound, fmt.Sprintf("No service configured for host: %s", sni))
		return
	}

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

	backendAddr := fmt.Sprintf("127.0.0.1:%d", tunnel.LocalPort())
	backendConn, err := net.DialTimeout("tcp", backendAddr, 10*time.Second)
	if err != nil {
		log.Printf("[tls] [%s] Failed to connect to backend: %v", sni, err)
		s.sendTLSErrorPage(conn.Conn, buf[:n], sni, tlsErrorBackendConnection, fmt.Sprintf("Failed to connect to backend: %v", err))
		return
	}
	defer backendConn.Close()

	// replay the ClientHello we already read - backend hasn't seen it yet
	if _, err := backendConn.Write(buf[:n]); err != nil {
		log.Printf("[tls] [%s] Failed to forward ClientHello: %v", sni, err)
		s.sendTLSErrorPage(conn.Conn, buf[:n], sni, tlsErrorForwarding, fmt.Sprintf("Failed to forward ClientHello: %v", err))
		return
	}

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

// extractSNI parses the TLS ClientHello to find the Server Name Indication.
// This is how we know which backend to route to before TLS terminates.
func extractSNI(data []byte) (string, error) {
	// TLS record: type(1) + version(2) + length(2) = 5 bytes minimum
	if len(data) < 5 {
		return "", fmt.Errorf("data too short for TLS record")
	}

	// Check if it's a TLS handshake (0x16)
	if data[0] != 0x16 {
		return "", fmt.Errorf("not a TLS handshake record")
	}

	recordLen := int(data[3])<<8 | int(data[4])
	if len(data) < 5+recordLen {
		return "", fmt.Errorf("incomplete TLS record")
	}

	handshake := data[5:]
	if len(handshake) < 4 {
		return "", fmt.Errorf("data too short for handshake header")
	}

	// Check if it's a ClientHello (0x01)
	if handshake[0] != 0x01 {
		return "", fmt.Errorf("not a ClientHello")
	}

	// skip: handshake header(4) + version(2) + random(32) = 38 bytes
	pos := 38
	if len(handshake) < pos+1 {
		return "", fmt.Errorf("data too short for session ID length")
	}

	sessionIDLen := int(handshake[pos])
	pos += 1 + sessionIDLen
	if len(handshake) < pos+2 {
		return "", fmt.Errorf("data too short for cipher suites length")
	}

	cipherSuitesLen := int(handshake[pos])<<8 | int(handshake[pos+1])
	pos += 2 + cipherSuitesLen
	if len(handshake) < pos+1 {
		return "", fmt.Errorf("data too short for compression methods length")
	}

	compressionLen := int(handshake[pos])
	pos += 1 + compressionLen
	if len(handshake) < pos+2 {
		return "", fmt.Errorf("no extensions present")
	}

	extensionsLen := int(handshake[pos])<<8 | int(handshake[pos+1])
	pos += 2
	extensionsEnd := pos + extensionsLen

	for pos+4 <= extensionsEnd && pos+4 <= len(handshake) {
		extType := int(handshake[pos])<<8 | int(handshake[pos+1])
		extLen := int(handshake[pos+2])<<8 | int(handshake[pos+3])
		pos += 4

		if pos+extLen > len(handshake) {
			break
		}

		// SNI is extension type 0x0000
		if extType == 0 {
			extData := handshake[pos : pos+extLen]
			if len(extData) < 5 {
				return "", fmt.Errorf("SNI extension too short")
			}
			// skip list length(2) + host type(1)
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
