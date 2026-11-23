package security

import "crypto/tls"

func TLSConfig(configTLSMin string) *tls.Config {
	tlsMin := uint16(tls.VersionTLS13)
	switch configTLSMin {
	case "1.2":
		tlsMin = tls.VersionTLS12
	}

	// Curves and cipher suites are left to Go's defaults, which include X25519 and the
	// X25519MLKEM768 post-quantum hybrid and are kept current with each Go release.
	// #nosec G402 -- disables G402: TLS MinVersion too low. (gosec)
	var TLSConfig = &tls.Config{
		MinVersion: tlsMin,
	}
	return TLSConfig
}
