package archives

import (
	"archive/zip"
	"compress/bzip2"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

// ZIP compression method IDs from APPNOTE.TXT 4.4.5.
const (
	zipMethodBzip2 uint16 = 12
	zipMethodZstd  uint16 = 93
	zipMethodXZ    uint16 = 95
)

func registerZipDecompressors(r *zip.Reader) {
	r.RegisterDecompressor(zip.Deflate, newFlateReader)
	r.RegisterDecompressor(zipMethodBzip2, newZipBzip2Reader)
	r.RegisterDecompressor(zipMethodZstd, newZipZstdReader)
	r.RegisterDecompressor(zipMethodXZ, newZipXZReader)
}

func newZipBzip2Reader(r io.Reader) io.ReadCloser {
	return io.NopCloser(bzip2.NewReader(r))
}

// maxZstdWindow is the largest window a zstd frame may declare. The decoder
// allocates the declared window, up to 512 MiB by default; zstd --ultra -22
// uses 128 MiB.
const maxZstdWindow = 128 << 20

func newZstdReader(r io.Reader) (io.ReadCloser, error) {
	dec, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(maxZstdWindow))
	if err != nil {
		return nil, err
	}
	return zstdReader{dec}, nil
}

// zstdReader reports a frame over maxZstdWindow as ErrDecompressLimit, like
// an xz dictionary over maxXZDictSize.
type zstdReader struct{ dec *zstd.Decoder }

func (z zstdReader) Read(p []byte) (int, error) {
	n, err := z.dec.Read(p)
	if errors.Is(err, zstd.ErrWindowSizeExceeded) || errors.Is(err, zstd.ErrDecoderSizeExceeded) {
		err = fmt.Errorf("%w: %w", ErrDecompressLimit, err)
	}
	return n, err
}

func (z zstdReader) Close() error {
	z.dec.Close()
	return nil
}

func newZipZstdReader(r io.Reader) io.ReadCloser {
	zr, err := newZstdReader(r)
	if err != nil {
		return errReadCloser{fmt.Errorf("opening zstd: %w", err)}
	}
	return zr
}

func newZipXZReader(r io.Reader) io.ReadCloser {
	xr, err := newXZReader(r)
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
