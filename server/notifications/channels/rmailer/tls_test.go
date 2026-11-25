package rmailer_test

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/notifications/channels/rmailer"
)

// fakeSMTP accepts one connection and reports the first thing the client did that tells its TLS mode: "tls" (a
// TLS handshake straight away: implicit TLS), "starttls" (it asked for STARTTLS after EHLO) or "plain" (it went on
// to MAIL FROM without TLS). The server offers STARTTLS.
func fakeSMTP(t *testing.T) (port int, seen <-chan string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	ch := make(chan string, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		// a TLS client speaks first (ClientHello, record type 0x16); an SMTP client waits for the greeting
		_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		b := make([]byte, 1)
		if n, _ := c.Read(b); n == 1 && b[0] == 0x16 {
			ch <- "tls"
			return
		}
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		r := bufio.NewReader(c)
		_, _ = c.Write([]byte("220 fake ESMTP\r\n"))
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				_, _ = c.Write([]byte("250-fake\r\n250-8BITMIME\r\n250 STARTTLS\r\n"))
			case cmd == "STARTTLS":
				ch <- "starttls"
				_, _ = c.Write([]byte("454 not today\r\n"))
				return
			case strings.HasPrefix(cmd, "MAIL FROM"):
				ch <- "plain"
				_, _ = c.Write([]byte("421 bye\r\n"))
				return
			default:
				_, _ = c.Write([]byte("250 ok\r\n"))
			}
		}
	}()
	return l.Addr().(*net.TCPAddr).Port, ch
}

func sendOnce(t *testing.T, port int, secure bool) {
	t.Helper()
	m := rmailer.NewRMailer(rmailer.Config{Host: "127.0.0.1", Port: port, Domain: "example.com", From: "procy@example.com", TLS: secure,
		AuthType: rmailer.AuthTypeNone, NoNoop: true}, testLog)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = m.Send(ctx, []string{"ann@example.com"}, "hi", rmailer.ContentTypeTextPlain, "hello")
}

// secure = false sends with STARTTLS when the server offers it (as the startup check of [smtp] does), e.g. a relay
// on port 587; secure = true is implicit TLS (port 465), as documented.
func TestSMTPTLSModes(t *testing.T) {
	port, seen := fakeSMTP(t)
	sendOnce(t, port, false)
	select {
	case got := <-seen:
		assert.Equal(t, "starttls", got)
	case <-time.After(5 * time.Second):
		t.Fatal("the client did nothing")
	}

	port, seen = fakeSMTP(t)
	sendOnce(t, port, true)
	select {
	case got := <-seen:
		assert.Equal(t, "tls", got)
	case <-time.After(5 * time.Second):
		t.Fatal("the client did nothing")
	}
}
