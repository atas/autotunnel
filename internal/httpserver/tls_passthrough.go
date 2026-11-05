package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/atas/autotunnel/internal/netutil"
)

// maxClientHelloSize limits how much a client can make us buffer before routing.
// crypto/tls has the same limit on handshake messages.
const maxClientHelloSize = 64 * 1024

var errIncompleteClientHello = errors.New("incomplete ClientHello")

func (s *Server) handleTLSConnection(conn *peekConn) {
	defer conn.Close()

	// give slow clients time to send ClientHello
	_ = conn.Conn.SetReadDeadline(time.Now().Add(TLSClientHelloDeadline))

	hello, err := readClientHello(conn)
	if err != nil {
		if s.config.Verbose {
			log.Printf("[tls] Error reading ClientHello: %v", err)
		}
		return
	}

	_ = conn.Conn.SetReadDeadline(time.Time{})

	sni, err := extractSNI(hello)
	if err != nil {
		log.Printf("[tls] Failed to extract SNI: %v", err)
		s.sendTLSErrorPage(conn.Conn, hello, "", tlsErrorSNIExtraction, fmt.Sprintf("Failed to extract SNI: %v", err))
		return
	}

	if s.config.Verbose {
		log.Printf("[tls] [%s] New connection", sni)
	}

	tunnel, err := s.manager.GetOrCreateTunnel(sni, "https")
	if err != nil {
		log.Printf("[tls] [%s] Error: %v", sni, err)
		s.sendTLSErrorPage(conn.Conn, hello, sni, tlsErrorRouteNotFound, fmt.Sprintf("No service configured for host: %s", sni))
		return
	}

	if !tunnel.IsRunning() {
		ctx, cancel := context.WithTimeout(context.Background(), TLSTunnelStartTimeout)
		if err := tunnel.Start(ctx); err != nil {
			cancel()
			log.Printf("[tls] [%s] Failed to start tunnel: %v", sni, err)
			s.sendTLSErrorPage(conn.Conn, hello, sni, tlsErrorTunnelStartup, fmt.Sprintf("Failed to start tunnel: %v", err))
			return
		}
		cancel()
	}

	tunnel.Touch()

	backendAddr := fmt.Sprintf("127.0.0.1:%d", tunnel.LocalPort())
	backendConn, err := net.DialTimeout("tcp", backendAddr, TLSBackendDialTimeout)
	if err != nil {
		log.Printf("[tls] [%s] Failed to connect to backend: %v", sni, err)
		// Port-forward is likely dead -- mark tunnel failed so next connection recreates it
		tunnel.MarkFailed(fmt.Errorf("backend dial failed: %w", err))
		s.sendTLSErrorPage(conn.Conn, hello, sni, tlsErrorBackendConnection, fmt.Sprintf("Failed to connect to backend: %v", err))
		return
	}
	defer backendConn.Close()

	// replay the ClientHello we already read - backend hasn't seen it yet
	if _, err := backendConn.Write(hello); err != nil {
		log.Printf("[tls] [%s] Failed to forward ClientHello: %v", sni, err)
		s.sendTLSErrorPage(conn.Conn, hello, sni, tlsErrorForwarding, fmt.Sprintf("Failed to forward ClientHello: %v", err))
		return
	}

	netutil.BidirectionalCopy(backendConn, conn.Conn)
}

// readClientHello reads until it has the whole ClientHello, which can arrive in
// several reads and span several TLS records. It returns everything it read, for
// replaying to the backend. Its buffer is at least as large as peekConn's, so each
// read empties peekConn and nothing is left behind. Data that cannot be a
// ClientHello is returned as well, for extractSNI to report.
func readClientHello(conn *peekConn) ([]byte, error) {
	var data []byte
	buf := make([]byte, 16384)
	for {
		n, err := conn.Read(buf)
		data = append(data, buf[:n]...)

		if _, parseErr := validateTLSHandshake(data); !errors.Is(parseErr, errIncompleteClientHello) {
			return data, nil
		}
		if err != nil {
			return nil, err
		}
		if len(data) > maxClientHelloSize {
			return nil, fmt.Errorf("ClientHello larger than %d bytes", maxClientHelloSize)
		}
	}
}

// extractSNI parses the TLS ClientHello to find the Server Name Indication.
// This is how we know which backend to route to before TLS terminates.
func extractSNI(data []byte) (string, error) {
	handshake, err := validateTLSHandshake(data)
	if err != nil {
		return "", err
	}

	extStart, extLen, err := skipToExtensions(handshake)
	if err != nil {
		return "", err
	}

	return findSNIInExtensions(handshake[extStart:], extLen)
}

// validateTLSHandshake validates TLS record and ClientHello headers, returning the
// ClientHello handshake message reassembled from its records. It returns
// errIncompleteClientHello if data ends before the message does.
func validateTLSHandshake(data []byte) ([]byte, error) {
	var handshake []byte
	for {
		// TLS record: type(1) + version(2) + length(2) = 5 bytes minimum
		if len(data) < 5 {
			return nil, errIncompleteClientHello
		}

		// Check if it's a TLS handshake (0x16)
		if data[0] != 0x16 {
			return nil, fmt.Errorf("not a TLS handshake record")
		}

		recordLen := int(data[3])<<8 | int(data[4])
		if len(data) < 5+recordLen {
			return nil, errIncompleteClientHello
		}
		handshake = append(handshake, data[5:5+recordLen]...)
		data = data[5+recordLen:]

		// Check if it's a ClientHello (0x01)
		if len(handshake) > 0 && handshake[0] != 0x01 {
			return nil, fmt.Errorf("not a ClientHello")
		}

		// Handshake header: type(1) + length(3)
		if len(handshake) >= 4 {
			msgLen := 4 + (int(handshake[1])<<16 | int(handshake[2])<<8 | int(handshake[3]))
			if len(handshake) >= msgLen {
				return handshake[:msgLen], nil
			}
		}
	}
}

// skipToExtensions skips the variable-length fields in ClientHello to find extensions
// Returns the offset to extensions start and total extensions length
func skipToExtensions(handshake []byte) (int, int, error) {
	// skip: handshake header(4) + version(2) + random(32) = 38 bytes
	pos := 38
	if len(handshake) < pos+1 {
		return 0, 0, fmt.Errorf("data too short for session ID length")
	}

	sessionIDLen := int(handshake[pos])
	pos += 1 + sessionIDLen
	if len(handshake) < pos+2 {
		return 0, 0, fmt.Errorf("data too short for cipher suites length")
	}

	cipherSuitesLen := int(handshake[pos])<<8 | int(handshake[pos+1])
	pos += 2 + cipherSuitesLen
	if len(handshake) < pos+1 {
		return 0, 0, fmt.Errorf("data too short for compression methods length")
	}

	compressionLen := int(handshake[pos])
	pos += 1 + compressionLen
	if len(handshake) < pos+2 {
		return 0, 0, fmt.Errorf("no extensions present")
	}

	extensionsLen := int(handshake[pos])<<8 | int(handshake[pos+1])
	return pos + 2, extensionsLen, nil
}

// findSNIInExtensions scans TLS extensions to find and parse the SNI extension
func findSNIInExtensions(extensions []byte, extLen int) (string, error) {
	pos := 0
	for pos+4 <= extLen && pos+4 <= len(extensions) {
		extType := int(extensions[pos])<<8 | int(extensions[pos+1])
		thisExtLen := int(extensions[pos+2])<<8 | int(extensions[pos+3])
		pos += 4

		if pos+thisExtLen > len(extensions) {
			break
		}

		// SNI is extension type 0x0000
		if extType == 0 {
			extData := extensions[pos : pos+thisExtLen]
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

		pos += thisExtLen
	}

	return "", fmt.Errorf("SNI extension not found")
}
