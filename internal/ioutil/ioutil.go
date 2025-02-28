// Copyright (c) 2015-2021 MinIO, Inc.
//
// This file is part of MinIO Object Storage stack
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

// Package ioutil implements some I/O utility functions which are not covered
// by the standard library.
package ioutil

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/minio/minio/internal/disk"
)

// Block sizes constant.
const (
	BlockSizeSmall       = 32 * humanize.KiByte // Default r/w block size for smaller objects.
	BlockSizeLarge       = 2 * humanize.MiByte  // Default r/w block size for larger objects.
	BlockSizeReallyLarge = 4 * humanize.MiByte  // Default write block size for objects per shard >= 64MiB
)

// aligned sync.Pool's
var (
	ODirectPoolXLarge = sync.Pool{
		New: func() interface{} {
			b := disk.AlignedBlock(BlockSizeReallyLarge)
			return &b
		},
	}
	ODirectPoolLarge = sync.Pool{
		New: func() interface{} {
			b := disk.AlignedBlock(BlockSizeLarge)
			return &b
		},
	}
	ODirectPoolSmall = sync.Pool{
		New: func() interface{} {
			b := disk.AlignedBlock(BlockSizeSmall)
			return &b
		},
	}
)

// WriteOnCloser implements io.WriteCloser and always
// executes at least one write operation if it is closed.
//
// This can be useful within the context of HTTP. At least
// one write operation must happen to send the HTTP headers
// to the peer.
type WriteOnCloser struct {
	io.Writer
	hasWritten bool
}

func (w *WriteOnCloser) Write(p []byte) (int, error) {
	w.hasWritten = true
	return w.Writer.Write(p)
}

// Close closes the WriteOnCloser. It behaves like io.Closer.
func (w *WriteOnCloser) Close() error {
	if !w.hasWritten {
		_, err := w.Write(nil)
		if err != nil {
			return err
		}
	}
	if closer, ok := w.Writer.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// HasWritten returns true if at least one write operation was performed.
func (w *WriteOnCloser) HasWritten() bool { return w.hasWritten }

// WriteOnClose takes an io.Writer and returns an ioutil.WriteOnCloser.
func WriteOnClose(w io.Writer) *WriteOnCloser {
	return &WriteOnCloser{w, false}
}

type ioret[V any] struct {
	val V
	err error
}

// DeadlineWriter deadline writer with timeout
type DeadlineWriter struct {
	io.WriteCloser
	timeout time.Duration
	err     error
}

// WithDeadline will execute a function with a deadline and return a value of a given type.
// If the deadline/context passes before the function finishes executing,
// the zero value and the context error is returned.
func WithDeadline[V any](ctx context.Context, timeout time.Duration, work func(ctx context.Context) (result V, err error)) (result V, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c := make(chan ioret[V], 1)
	go func() {
		v, err := work(ctx)
		c <- ioret[V]{val: v, err: err}
	}()

	select {
	case v := <-c:
		return v.val, v.err
	case <-ctx.Done():
		var zero V
		return zero, ctx.Err()
	}
}

// DeadlineWorker implements the deadline/timeout resiliency pattern.
type DeadlineWorker struct {
	timeout time.Duration
}

// NewDeadlineWorker constructs a new DeadlineWorker with the given timeout.
// To return values, use the WithDeadline helper instead.
func NewDeadlineWorker(timeout time.Duration) *DeadlineWorker {
	dw := &DeadlineWorker{
		timeout: timeout,
	}
	return dw
}

// Run runs the given function, passing it a stopper channel. If the deadline passes before
// the function finishes executing, Run returns context.DeadlineExceeded to the caller.
// channel so that the work function can attempt to exit gracefully.
// Multiple calls to Run will run independently of each other.
func (d *DeadlineWorker) Run(work func() error) error {
	c := make(chan ioret[struct{}], 1)
	t := time.NewTimer(d.timeout)
	go func() {
		c <- ioret[struct{}]{val: struct{}{}, err: work()}
	}()

	select {
	case r := <-c:
		if !t.Stop() {
			<-t.C
		}
		return r.err
	case <-t.C:
		return context.DeadlineExceeded
	}
}

// NewDeadlineWriter wraps a writer to make it respect given deadline
// value per Write(). If there is a blocking write, the returned Writer
// will return whenever the timer hits (the return values are n=0
// and err=context.DeadlineExceeded.)
func NewDeadlineWriter(w io.WriteCloser, timeout time.Duration) io.WriteCloser {
	return &DeadlineWriter{WriteCloser: w, timeout: timeout}
}

func (w *DeadlineWriter) Write(buf []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}

	c := make(chan ioret[int], 1)
	t := time.NewTimer(w.timeout)
	go func() {
		n, err := w.WriteCloser.Write(buf)
		c <- ioret[int]{val: n, err: err}
		close(c)
	}()

	select {
	case r := <-c:
		if !t.Stop() {
			<-t.C
		}
		w.err = r.err
		return r.val, r.err
	case <-t.C:
		w.WriteCloser.Close()
		w.err = context.DeadlineExceeded
		return 0, context.DeadlineExceeded
	}
}

// Close closer interface to close the underlying closer
func (w *DeadlineWriter) Close() error {
	return w.WriteCloser.Close()
}

// LimitWriter implements io.WriteCloser.
//
// This is implemented such that we want to restrict
// an enscapsulated writer upto a certain length
// and skip a certain number of bytes.
type LimitWriter struct {
	io.Writer
	skipBytes int64
	wLimit    int64
}

// Write implements the io.Writer interface limiting upto
// configured length, also skips the first N bytes.
func (w *LimitWriter) Write(p []byte) (n int, err error) {
	n = len(p)
	var n1 int
	if w.skipBytes > 0 {
		if w.skipBytes >= int64(len(p)) {
			w.skipBytes -= int64(len(p))
			return n, nil
		}
		p = p[w.skipBytes:]
		w.skipBytes = 0
	}
	if w.wLimit == 0 {
		return n, nil
	}
	if w.wLimit < int64(len(p)) {
		n1, err = w.Writer.Write(p[:w.wLimit])
		w.wLimit -= int64(n1)
		return n, err
	}
	n1, err = w.Writer.Write(p)
	w.wLimit -= int64(n1)
	return n, err
}

// Close closes the LimitWriter. It behaves like io.Closer.
func (w *LimitWriter) Close() error {
	if closer, ok := w.Writer.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// LimitedWriter takes an io.Writer and returns an ioutil.LimitWriter.
func LimitedWriter(w io.Writer, skipBytes int64, limit int64) *LimitWriter {
	return &LimitWriter{w, skipBytes, limit}
}

type nopCloser struct {
	io.Writer
}

func (nopCloser) Close() error { return nil }

// NopCloser returns a WriteCloser with a no-op Close method wrapping
// the provided Writer w.
func NopCloser(w io.Writer) io.WriteCloser {
	return nopCloser{w}
}

// SkipReader skips a given number of bytes and then returns all
// remaining data.
type SkipReader struct {
	io.Reader

	skipCount int64
}

func (s *SkipReader) Read(p []byte) (int, error) {
	l := int64(len(p))
	if l == 0 {
		return 0, nil
	}
	for s.skipCount > 0 {
		if l > s.skipCount {
			l = s.skipCount
		}
		n, err := s.Reader.Read(p[:l])
		if err != nil {
			return 0, err
		}
		s.skipCount -= int64(n)
	}
	return s.Reader.Read(p)
}

// NewSkipReader - creates a SkipReader
func NewSkipReader(r io.Reader, n int64) io.Reader {
	return &SkipReader{r, n}
}

var copyBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 32*1024)
		return &b
	},
}

// Copy is exactly like io.Copy but with reusable buffers.
func Copy(dst io.Writer, src io.Reader) (written int64, err error) {
	bufp := copyBufPool.Get().(*[]byte)
	buf := *bufp
	defer copyBufPool.Put(bufp)

	return io.CopyBuffer(dst, src, buf)
}

// SameFile returns if the files are same.
func SameFile(fi1, fi2 os.FileInfo) bool {
	if !os.SameFile(fi1, fi2) {
		return false
	}
	if !fi1.ModTime().Equal(fi2.ModTime()) {
		return false
	}
	if fi1.Mode() != fi2.Mode() {
		return false
	}
	return fi1.Size() == fi2.Size()
}

// DirectioAlignSize - DirectIO alignment needs to be 4K. Defined here as
// directio.AlignSize is defined as 0 in MacOS causing divide by 0 error.
const DirectioAlignSize = 4096

// CopyAligned - copies from reader to writer using the aligned input
// buffer, it is expected that input buffer is page aligned to
// 4K page boundaries. Without passing aligned buffer may cause
// this function to return error.
//
// This code is similar in spirit to io.Copy but it is only to be
// used with DIRECT I/O based file descriptor and it is expected that
// input writer *os.File not a generic io.Writer. Make sure to have
// the file opened for writes with syscall.O_DIRECT flag.
func CopyAligned(w io.Writer, r io.Reader, alignedBuf []byte, totalSize int64, file *os.File) (int64, error) {
	if totalSize == 0 {
		return 0, nil
	}

	// Writes remaining bytes in the buffer.
	writeUnaligned := func(w io.Writer, buf []byte) (remainingWritten int64, err error) {
		// Disable O_DIRECT on fd's on unaligned buffer
		// perform an amortized Fdatasync(fd) on the fd at
		// the end, this is performed by the caller before
		// closing 'w'.
		if err = disk.DisableDirectIO(file); err != nil {
			return remainingWritten, err
		}
		// Since w is *os.File io.Copy shall use ReadFrom() call.
		return io.Copy(w, bytes.NewReader(buf))
	}

	var written int64
	for {
		buf := alignedBuf
		if totalSize > 0 {
			remaining := totalSize - written
			if remaining < int64(len(buf)) {
				buf = buf[:remaining]
			}
		}

		nr, err := io.ReadFull(r, buf)
		eof := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
		if err != nil && !eof {
			return written, err
		}

		buf = buf[:nr]
		var nw int64
		if len(buf)%DirectioAlignSize == 0 {
			var n int
			// buf is aligned for directio write()
			n, err = w.Write(buf)
			nw = int64(n)
		} else {
			// buf is not aligned, hence use writeUnaligned()
			nw, err = writeUnaligned(w, buf)
		}

		if nw > 0 {
			written += nw
		}

		if err != nil {
			return written, err
		}

		if nw != int64(len(buf)) {
			return written, io.ErrShortWrite
		}

		if totalSize > 0 && written == totalSize {
			// we have written the entire stream, return right here.
			return written, nil
		}

		if eof {
			// We reached EOF prematurely but we did not write everything
			// that we promised that we would write.
			if totalSize > 0 && written != totalSize {
				return written, io.ErrUnexpectedEOF
			}
			return written, nil
		}
	}
}
