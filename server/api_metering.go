package chserver

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/felixge/httpsnoop"

	"github.com/openrport/openrport/server/metering"
	"github.com/openrport/openrport/share/tenant"
)

// meterResponses counts the bytes of every response (websockets included, through the hijacked connection) as
// api_bytes_out of the request's tenant (usage metering). Requests of reserved tenants (ids starting with "_": the
// operators and unresolved requests) are not counted.
func meterResponses(m *metering.Meter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tid, err := tenant.FromContext(r.Context())
			if err != nil || strings.HasPrefix(string(tid), "_") {
				next.ServeHTTP(w, r)
				return
			}
			count := func(n int) {
				if n > 0 {
					m.Add(tid, "", metering.APIBytesOut, int64(n))
				}
			}
			next.ServeHTTP(httpsnoop.Wrap(w, httpsnoop.Hooks{
				Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
					return func(b []byte) (int, error) {
						n, err := next(b)
						count(n)
						return n, err
					}
				},
				WriteString: func(next httpsnoop.WriteStringFunc) httpsnoop.WriteStringFunc {
					return func(s string) (int, error) {
						n, err := next(s)
						count(n)
						return n, err
					}
				},
				ReadFrom: func(next httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
					return func(src io.Reader) (int64, error) {
						n, err := next(src)
						count(int(n))
						return n, err
					}
				},
				Hijack: func(next httpsnoop.HijackFunc) httpsnoop.HijackFunc {
					return func() (net.Conn, *bufio.ReadWriter, error) {
						c, rw, err := next()
						if err != nil {
							return c, rw, err
						}
						cc := &countingConn{Conn: c, count: count}
						// writes through the buffered writer reach the connection through the counting one
						rw.Writer.Reset(cc)
						return cc, rw, nil
					}
				},
			}), r)
		})
	}
}

// countingConn counts the bytes written to a hijacked connection.
type countingConn struct {
	net.Conn
	count func(n int)
}

func (c *countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.count(n)
	return n, err
}
