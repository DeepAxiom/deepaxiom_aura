package wtsrv

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

// Handler receives one established session. It owns the session from that
// point and must Close it when done — the same contract the WebSocket handlers
// already have with their connections.
type Handler func(ctx context.Context, s *Session, r *http.Request)

// Server is the QUIC/HTTP3 listener.
//
// It runs *beside* the TCP listener rather than replacing it. Three reasons,
// and they are not hedging: UDP is blocked on enough corporate networks that a
// QUIC-only node would be unreachable from exactly the places this runtime is
// meant to install; the conformance suite and every existing client speak
// WebSocket; and a transport upgrade that forces a flag day is one nobody
// takes. A node offers both and a peer picks.
type Server struct {
	// Addr is the UDP address, normally the same port number as the TCP
	// listener. Same number, different protocol — they do not collide.
	Addr string
	// DataDir is where the self-signed certificate is kept when none is given.
	DataDir string
	// TLS, when set, is used as-is (a real certificate for a real hostname).
	// When nil the server loads or creates a self-signed one.
	TLS *tls.Config
	Log *slog.Logger

	// Routes maps a path to its handler, mirroring the HTTP surface:
	// "/ws/skill" for skills, "/v1/stream" for clients.
	Routes map[string]Handler

	srv   *http3.Server
	wt    *webtransport.Server
	cert  tls.Certificate
	local string
}

// LocalAddr is the address actually bound, which is not Addr when Addr asked
// for port 0. Callers that print an endpoint, and every test, need the real
// one.
func (s *Server) LocalAddr() string { return s.local }

// Start begins listening. It returns once the socket is open, so a caller can
// report the endpoint before serving blocks.
func (s *Server) Start() error {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	tlsConf := s.TLS
	if tlsConf == nil {
		cert, err := loadOrCreateCert(s.DataDir)
		if err != nil {
			return fmt.Errorf("webtransport certificate: %w", err)
		}
		s.cert = cert
		tlsConf = &tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{http3.NextProtoH3},
			MinVersion:   tls.VersionTLS13,
		}
	}

	mux := http.NewServeMux()
	s.wt = &webtransport.Server{
		H3: &http3.Server{
			Addr:            s.Addr,
			TLSConfig:       tlsConf,
			Handler:         mux,
			EnableDatagrams: true,
			QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
		},
		// The origin check is deliberately permissive here and enforced one
		// layer up: the gateway's auth middleware already owns origin policy
		// for the WebSocket path, and two places deciding the same question is
		// how they end up disagreeing.
		CheckOrigin: func(*http.Request) bool { return true },
	}
	s.srv = s.wt.H3

	for path, h := range s.Routes {
		handler := h
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			sess, err := s.wt.Upgrade(w, r)
			if err != nil {
				s.Log.Debug("wtsrv: upgrade refused", "path", r.URL.Path, "err", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			// The client opens the control stream immediately after the
			// upgrade; accepting it here means a peer that connects and then
			// says nothing is dropped rather than parked forever.
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			ctrl, err := sess.AcceptStream(ctx)
			if err != nil {
				s.Log.Debug("wtsrv: no control stream", "err", err)
				_ = sess.CloseWithError(1, "expected a control stream")
				return
			}
			wrapped := newSession(sess, ctrl, func(msg string, args ...any) {
				s.Log.Debug(msg, args...)
			})
			defer wrapped.Close()
			handler(r.Context(), wrapped, r)
		})
	}

	conn, err := net.ListenPacket("udp", s.Addr)
	if err != nil {
		return fmt.Errorf("listen udp %s: %w", s.Addr, err)
	}
	s.local = conn.LocalAddr().String()
	// The WebTransport server's own Serve, not the HTTP/3 one underneath it:
	// it demultiplexes WebTransport streams away from HTTP/3 request streams
	// and advertises the SETTINGS that tell a client this endpoint speaks the
	// protocol at all. Serving the inner http3.Server directly gets a session
	// that establishes and then rejects every stream on it.
	go func() {
		if err := s.wt.Serve(conn); err != nil {
			s.Log.Debug("wtsrv: serve ended", "err", err)
		}
	}()
	return nil
}

// Close stops the listener.
func (s *Server) Close() error {
	if s.wt != nil {
		return s.wt.Close()
	}
	return nil
}

// ── the certificate ─────────────────────────────────────────────────

const (
	certFile = "webtransport-cert.pem"
	keyFile  = "webtransport-key.pem"
	// certValidity is 14 days rather than the usual year. WebTransport lets a
	// browser accept a self-signed certificate by hash, but only one valid for
	// no more than 14 days — the trade the spec makes for allowing it at all.
	// A node therefore rotates rather than issuing something long-lived.
	certValidity = 14 * 24 * time.Hour
)

// loadOrCreateCert reuses the node's certificate, minting a new one when it is
// missing or within a day of expiry.
//
// Auto-generation rather than a required flag, for the same reason the node
// generates its own bearer token: QUIC has no cleartext mode, so demanding a
// certificate would make `aura up` fail on a laptop for a reason that has
// nothing to do with what the operator was trying to do.
func loadOrCreateCert(dataDir string) (tls.Certificate, error) {
	if dataDir == "" {
		return newCert()
	}
	cp := filepath.Join(dataDir, certFile)
	kp := filepath.Join(dataDir, keyFile)

	if cert, err := tls.LoadX509KeyPair(cp, kp); err == nil {
		if leaf, err := x509.ParseCertificate(cert.Certificate[0]); err == nil {
			if time.Now().Before(leaf.NotAfter.Add(-24 * time.Hour)) {
				cert.Leaf = leaf
				return cert, nil
			}
		}
	}

	cert, err := newCert()
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return cert, nil // usable in memory even if it cannot be persisted
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return cert, nil
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	_ = os.WriteFile(cp, certPEM, 0o644)
	// The key is 0600 for the same reason node.token is: a credential another
	// user on the machine can read is not a credential.
	_ = os.WriteFile(kp, keyPEM, 0o600)
	return cert, nil
}

func newCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "aura-node", Organization: []string{"Deep Axiom"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost", "aura-node"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}, nil
}

// fingerprint renders a certificate's SHA-256 as colon-separated hex, the form
// `serverCertificateHashes` and every TLS tool already use.
func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	out := make([]byte, 0, len(sum)*3)
	for i, b := range sum {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, hexDigits[b>>4], hexDigits[b&0x0f])
	}
	return string(out)
}

const hexDigits = "0123456789ABCDEF"
