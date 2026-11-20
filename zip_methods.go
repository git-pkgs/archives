package archives

import (
	"archive/zip"
	"compress/bzip2"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// ZIP compression method IDs from APPNOTE.TXT 4.4.5.
const (
	zipMethodBzip2 uint16 = 12
	zipMethodZstd  uint16 = 93
	zipMethodXZ    uint16 = 95
)

// registerZipDecompressors gives every ZIP reader the same methods, so
// buffered, streaming, random-access and browser access agree.
func registerZipDecompressors(r *zip.Reader) {
	r.RegisterDecompressor(zip.Deflate, newFlateReader)
	r.RegisterDecompressor(zipMethodBzip2, newZipBzip2Reader)
	r.RegisterDecompressor(zipMethodZstd, newZipZstdReader)
	r.RegisterDecompressor(zipMethodXZ, newZipXZReader)
}

func newZipBzip2Reader(r io.Reader) io.ReadCloser {
	return io.NopCloser(bzip2.NewReader(r))
}

func newZipZstdReader(r io.Reader) io.ReadCloser {
	dec, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return errReadCloser{fmt.Errorf("opening zstd: %w", err)}
	}
	return dec.IOReadCloser()
}

func newZipXZReader(r io.Reader) io.ReadCloser {
	xr, err := xz.NewReader(r)
	if err != nil {
		return errReadCloser{fmt.Errorf("opening xz: %w", err)}
	}
	return io.NopCloser(xr)
}

// errReadCloser reports a decoder that could not start. archive/zip treats
// a nil decompressor result as an unsupported method, so return the error
// from Read instead.
type errReadCloser struct{ err error }

func (e errReadCloser) Read([]byte) (int, error) { return 0, e.err }
func (e errReadCloser) Close() error             { return nil }
