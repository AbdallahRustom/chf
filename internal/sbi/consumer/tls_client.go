package consumer

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

type ClientTLSConfig struct {
	// mTLS client credentials (both required for mTLS)
	ClientCertFile string // PEM
	ClientKeyFile  string // PEM

	// Server trust (optional; if empty, uses system roots)
	RootCAFiles []string // one or more PEM CA files

	// TLS verification controls
	ServerName         string // optional override for SNI/hostname verification
	InsecureSkipVerify bool   // avoid in production

	// Hardening
	MinVersionTLS13 bool // if true, require TLS 1.3; otherwise TLS 1.2+

	// HTTP client behavior
	Timeout time.Duration // default 30s
}

// NewMTLSHTTPClientFromFiles builds an http.Client that uses mutual TLS and custom roots.
func NewMTLSHTTPClientFromFiles(cfg ClientTLSConfig) (*http.Client, error) {
	// Build Root CA pool
	var rootCAs *x509.CertPool
	if len(cfg.RootCAFiles) > 0 {
		rootCAs = x509.NewCertPool()
		for _, caPath := range cfg.RootCAFiles {
			if strings.TrimSpace(caPath) == "" {
				continue
			}
			pem, err := os.ReadFile(caPath)
			if err != nil {
				return nil, fmt.Errorf("read CA %q: %w", caPath, err)
			}
			if ok := rootCAs.AppendCertsFromPEM(pem); !ok {
				return nil, fmt.Errorf("append CA %q: not valid PEM", caPath)
			}
		}
	} else {
		// Use system roots if no CA provided
		var err error
		rootCAs, err = x509.SystemCertPool()
		if err != nil {
			// Fall back to empty pool (works if server chain anchors in system store on this OS)
			rootCAs = x509.NewCertPool()
		}
	}

	// Load client cert/key for mTLS (both must be present)
	if strings.TrimSpace(cfg.ClientCertFile) == "" || strings.TrimSpace(cfg.ClientKeyFile) == "" {
		return nil, fmt.Errorf("client cert and key files are required for mTLS")
	}
	cert, err := tls.LoadX509KeyPair(cfg.ClientCertFile, cfg.ClientKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load client key pair: %w", err)
	}

	tlsCfg := &tls.Config{
		MinVersion: func() uint16 {
			if cfg.MinVersionTLS13 {
				return tls.VersionTLS13
			}
			return tls.VersionTLS12
		}(),
		Certificates:       []tls.Certificate{cert},
		RootCAs:            rootCAs,
		ServerName:         strings.TrimSpace(cfg.ServerName), // optional
		InsecureSkipVerify: false,                             // not recommended in prod
	}

	tr := &http.Transport{
		TLSClientConfig:   tlsCfg,
		Proxy:             http.ProxyFromEnvironment,
		ForceAttemptHTTP2: true,
		MaxIdleConns:      100,
		IdleConnTimeout:   90 * time.Second,
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
	}, nil
}
