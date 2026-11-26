package clienttunnel

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pohcee/procy-server/server/metering"
)

type rwc struct {
	io.Reader
	io.Writer
}

func (rwc) Close() error { return nil }

func TestCountingChannelCountsBothDirections(t *testing.T) {
	m := metering.New(metering.Options{Writer: nil})
	b := m.Tunnel("a", "c1")
	var sent bytes.Buffer
	c := &countingChannel{ReadWriteCloser: rwc{Reader: bytes.NewReader(make([]byte, 5000)), Writer: &sent}, bytes: b}
	n, err := io.Copy(io.Discard, c) // the device's data toward the user
	assert.NoError(t, err)
	assert.EqualValues(t, 5000, n)
	_, err = io.Copy(c, bytes.NewReader(make([]byte, 700))) // the user's data toward the device
	assert.NoError(t, err)
	toUser, toDevice := b.Totals()
	assert.EqualValues(t, 5000, toUser)
	assert.EqualValues(t, 700, toDevice)
	assert.Equal(t, 700, sent.Len())
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { return len(p), nil }

// BenchmarkTunnelCopy compares copying through a tunnel's channel with and without counting (32 KiB buffers, as
// io.Copy in chshare.Pipe): the counting adds one atomic add per buffer.
func BenchmarkTunnelCopy(b *testing.B) {
	m := metering.New(metering.Options{})
	for _, metered := range []bool{false, true} {
		name := "plain"
		if metered {
			name = "metered"
		}
		b.Run(name, func(b *testing.B) {
			var ch io.ReadWriteCloser = rwc{Reader: zeroReader{}, Writer: io.Discard}
			if metered {
				ch = &countingChannel{ReadWriteCloser: ch, bytes: m.Tunnel("a", "c1")}
			}
			buf := make([]byte, 32<<10)
			b.SetBytes(int64(len(buf)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				n, _ := ch.Read(buf)
				_, _ = ch.Write(buf[:n])
			}
		})
	}
}
