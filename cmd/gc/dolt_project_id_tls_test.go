package main

import (
	"crypto/tls"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVerifiedDoltTLSConfigRequiresTrustedMatchingCertificate(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	host, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(t.TempDir(), "private-ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	address := net.JoinHostPort(host, port)

	privateCA, err := verifiedDoltTLSConfig(host, caPath)
	if err != nil {
		t.Fatalf("verifiedDoltTLSConfig(private CA): %v", err)
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, privateCA)
	if err != nil {
		t.Fatalf("private CA handshake failed: %v", err)
	}
	_ = conn.Close()

	wrongName, err := verifiedDoltTLSConfig("wrong.invalid", caPath)
	if err != nil {
		t.Fatal(err)
	}
	if conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, wrongName); err == nil {
		_ = conn.Close()
		t.Fatal("TLS accepted a certificate for a different server name")
	}

	systemOnly, err := verifiedDoltTLSConfig(host, "")
	if err != nil {
		t.Fatal(err)
	}
	if conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, systemOnly); err == nil {
		_ = conn.Close()
		t.Fatal("TLS accepted a self-signed certificate absent from system roots")
	}
}

func TestVerifiedDoltTLSConfigDoesNotDowngradeToPlaintext(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			var first [1]byte
			_, _ = conn.Read(first[:])
			_ = conn.Close()
		}
	}()
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	config, err := verifiedDoltTLSConfig(host, "")
	if err != nil {
		t.Fatal(err)
	}
	if conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", net.JoinHostPort(host, port), config); err == nil {
		_ = conn.Close()
		t.Fatal("TLS client accepted a plaintext endpoint")
	}
}
