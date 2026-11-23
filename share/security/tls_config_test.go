package security

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// selfSignedCert returns a throwaway certificate for 127.0.0.1.
func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// handshake connects a client offering only the given curves to a server using TLSConfig.
func handshake(t *testing.T, curves []tls.CurveID) (tls.CurveID, error) {
	t.Helper()
	srvCfg := TLSConfig("1.3")
	srvCfg.Certificates = []tls.Certificate{selfSignedCert(t)}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvCfg)
	require.NoError(t, err)
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.(*tls.Conn).Handshake()
	}()

	// #nosec G402 -- test client against a throwaway self-signed certificate.
	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{
		InsecureSkipVerify: true,
		CurvePreferences:   curves,
		MinVersion:         tls.VersionTLS13,
	})
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	return conn.ConnectionState().CurveID, nil
}

func TestTLSConfigAcceptsX25519(t *testing.T) {
	curve, err := handshake(t, []tls.CurveID{tls.X25519})
	require.NoError(t, err)
	require.Equal(t, tls.X25519, curve)
}

func TestTLSConfigAcceptsPostQuantumHybrid(t *testing.T) {
	curve, err := handshake(t, []tls.CurveID{tls.X25519MLKEM768})
	require.NoError(t, err)
	require.Equal(t, tls.X25519MLKEM768, curve)
}
