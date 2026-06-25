package proxy

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"time"
)

// peekClientHello reads the TLS ClientHello from conn WITHOUT consuming it,
// returning the SNI server name and a replacement net.Conn that replays the
// peeked bytes followed by the rest of the connection. This is how transparent
// capture decides MITM-vs-splice on the SNI before terminating TLS (plan §9).
//
// The technique drives a throwaway tls.Server over a read-only, recording view
// of the connection; the handshake aborts right after the ClientHello is parsed
// (the read-only conn refuses writes), but GetConfigForClient has already
// captured the ServerName.
func peekClientHello(conn net.Conn) (sni string, replay net.Conn, err error) {
	var buf bytes.Buffer
	hello, herr := readClientHello(io.TeeReader(conn, &buf))
	replay = &prependConn{Conn: conn, r: io.MultiReader(bytes.NewReader(buf.Bytes()), conn)}
	if herr != nil {
		return "", replay, herr
	}
	return hello.ServerName, replay, nil
}

func readClientHello(r io.Reader) (*tls.ClientHelloInfo, error) {
	var hello *tls.ClientHelloInfo
	err := tls.Server(readOnlyConn{r: r}, &tls.Config{
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			hello = &tls.ClientHelloInfo{ServerName: h.ServerName}
			return nil, nil
		},
	}).Handshake()
	if hello == nil {
		if err == nil {
			err = errors.New("proxy: no ClientHello observed")
		}
		return nil, err
	}
	// err is expected (handshake can't complete over a read-only conn); ignore it.
	return hello, nil
}

// readOnlyConn exposes a reader as a net.Conn whose writes fail, so a tls.Server
// handshake stops right after reading the ClientHello.
type readOnlyConn struct{ r io.Reader }

func (c readOnlyConn) Read(p []byte) (int, error)     { return c.r.Read(p) }
func (readOnlyConn) Write([]byte) (int, error)        { return 0, io.ErrClosedPipe }
func (readOnlyConn) Close() error                     { return nil }
func (readOnlyConn) LocalAddr() net.Addr              { return nil }
func (readOnlyConn) RemoteAddr() net.Addr             { return nil }
func (readOnlyConn) SetDeadline(time.Time) error      { return nil }
func (readOnlyConn) SetReadDeadline(time.Time) error  { return nil }
func (readOnlyConn) SetWriteDeadline(time.Time) error { return nil }

// prependConn replays buffered bytes before reading from the underlying conn.
type prependConn struct {
	net.Conn
	r io.Reader
}

func (c *prependConn) Read(p []byte) (int, error) { return c.r.Read(p) }
