package proxy

import (
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"
)

func TestPeekClientHelloSNI(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	// A real client writes a ClientHello with the given SNI; its handshake will
	// not complete (no server response), which is fine — we only need the hello.
	go func() {
		_ = tls.Client(client, &tls.Config{ServerName: "claude.ai", InsecureSkipVerify: true}).Handshake()
	}()

	server.SetReadDeadline(time.Now().Add(5 * time.Second))
	sni, replay, err := peekClientHello(server)
	if err != nil {
		t.Fatalf("peekClientHello error: %v", err)
	}
	if sni != "claude.ai" {
		t.Fatalf("SNI = %q, want claude.ai", sni)
	}

	// The replay conn must yield the original ClientHello bytes (starting with
	// the TLS handshake record type 0x16).
	first := make([]byte, 1)
	replay.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(replay, first); err != nil {
		t.Fatalf("replay read: %v", err)
	}
	if first[0] != 0x16 {
		t.Errorf("replay first byte = 0x%02x, want 0x16 (TLS handshake)", first[0])
	}
}

func TestPeekClientHelloNonTLS(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		client.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
		client.Close()
	}()

	server.SetReadDeadline(time.Now().Add(2 * time.Second))
	sni, _, err := peekClientHello(server)
	if err == nil && sni != "" {
		t.Errorf("expected no SNI for non-TLS input, got %q", sni)
	}
}
