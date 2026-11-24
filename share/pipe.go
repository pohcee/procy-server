package chshare

import (
	"io"
	"sync"
)

func Pipe(src io.ReadWriteCloser, dst io.ReadWriteCloser) (int64, int64) {
	var sent, received int64
	var wg sync.WaitGroup
	var o sync.Once
	closeFn := func() {
		src.Close()
		dst.Close()
	}
	wg.Add(2)
	go func() {
		received, _ = io.Copy(src, dst)
		o.Do(closeFn)
		wg.Done()
	}()
	go func() {
		sent, _ = io.Copy(dst, src)
		o.Do(closeFn)
		wg.Done()
	}()
	wg.Wait()
	return sent, received
}
