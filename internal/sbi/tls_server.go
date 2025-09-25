package sbi

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

func (s *Server) ListenAndServeMutalTLS(serverCertFile, serverKeyFile, clientCAFile string) error {
	// Load server certificate
	cert, err := tls.LoadX509KeyPair(serverCertFile, serverKeyFile)
	if err != nil {
		return fmt.Errorf("load server key pair: %w", err)
	}

	// Load CA cert to verify client certs
	caPEM, err := os.ReadFile(clientCAFile)
	if err != nil {
		return fmt.Errorf("read client CA file: %w", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("client CA file is not valid PEM")
	}

	// Prepare TLS config
	if s.httpServer.TLSConfig == nil {
		s.httpServer.TLSConfig = &tls.Config{}
	}
	s.httpServer.TLSConfig.MinVersion = tls.VersionTLS12
	s.httpServer.TLSConfig.PreferServerCipherSuites = true

	// Present this server certificate
	s.httpServer.TLSConfig.Certificates = []tls.Certificate{cert}

	// Require and verify client certificates (full mTLS)
	s.httpServer.TLSConfig.ClientAuth = tls.RequireAndVerifyClientCert
	s.httpServer.TLSConfig.ClientCAs = clientCAs

	// Use in-memory certs from TLSConfig
	return s.httpServer.ListenAndServeTLS("", "")
}
