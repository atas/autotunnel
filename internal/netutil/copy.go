package netutil

import (
	"io"
	"net"
	"sync"
)

// BidirectionalCopy copies data between two connections in both directions.
// It blocks until both directions are complete and handles CloseWrite for TCP connections.
func BidirectionalCopy(conn1, conn2 net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	copy := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}

	go copy(conn1, conn2)
	go copy(conn2, conn1)

	wg.Wait()
}
