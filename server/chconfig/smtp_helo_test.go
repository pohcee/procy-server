package chconfig

import (
	"bufio"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeRelay answers like Google's SMTP relay: it refuses a greeting from "localhost" with 421 and accepts any
// other name. It returns the address and a channel with the name each client greeted with.
func fakeRelay(t *testing.T) (string, <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	names := make(chan string, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				_, _ = c.Write([]byte("220 relay ready\r\n"))
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					cmd := strings.TrimSpace(line)
					upper := strings.ToUpper(cmd)
					switch {
					case strings.HasPrefix(upper, "EHLO ") || strings.HasPrefix(upper, "HELO "):
						name := strings.TrimSpace(cmd[5:])
						names <- name
						if name == "localhost" {
							_, _ = c.Write([]byte("421 4.7.0 Try again later, closing connection. (EHLO)\r\n"))
							return
						}
						_, _ = c.Write([]byte("250-relay\r\n250 8BITMIME\r\n"))
					case upper == "QUIT":
						_, _ = c.Write([]byte("221 bye\r\n"))
						return
					default:
						_, _ = c.Write([]byte("250 ok\r\n"))
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), names
}

func TestSMTPValidateGreetsWithTheSenderDomain(t *testing.T) {
	addr, names := fakeRelay(t)
	c := &SMTPConfig{Server: addr, SenderEmail: "procy@procy.sh"}

	require.NoError(t, c.Validate())
	require.Equal(t, "procy.sh", <-names)
}
