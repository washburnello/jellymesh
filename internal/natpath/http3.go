package natpath

import (
	"net/http"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

// Media travels over a direct path as the same HTTP requests, to the same
// handlers, as over the TCP federation connection, only framed as HTTP/3.
// The source's handler sees the caller's pinned key in request.TLS exactly
// as on TCP, so its authorization, per-destination ceiling, and range
// handling apply unchanged (C-NT-5).

// Serve answers HTTP/3 requests arriving on an accepted direct connection
// with handler, until the connection ends.
func Serve(conn *quic.Conn, handler http.Handler) error {
	return (&http3.Server{Handler: handler}).ServeQUICConn(conn)
}

// RoundTripper sends HTTP requests over a dialed direct connection.
func RoundTripper(conn *quic.Conn) http.RoundTripper {
	return (&http3.Transport{}).NewClientConn(conn)
}
