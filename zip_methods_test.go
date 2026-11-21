package archives

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"hash/crc32"
	"io"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

var zipMethodPayload = []byte(strings.Repeat("archives zip method test\n", 50))

// Go has no bzip2 encoder: bz2.compress(zipMethodPayload, 9) from CPython.
const zipMethodPayloadBzip2 = "QlpoOTFBWSZTWb3KE+YAAHzRgAAQQAAuYt0QIABgKVU9TIaaeKFMABNMou0XpFlFwiyiyi0i9otIu0WEW0WkX5F4RbRaRbIsouUWEXCLCL+LuSKcKEhe5QnzAA=="

var zipMethods = []struct {
	name   string
	method uint16
}{
	{"bzip2", zipMethodBzip2},
	{"zstd", zipMethodZstd},
	{"xz", zipMethodXZ},
}

func compressForZip(t *testing.T, method uint16, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	switch method {
	case zipMethodBzip2:
		raw, err := base64.StdEncoding.DecodeString(zipMethodPayloadBzip2)
		if err != nil || !bytes.Equal(payload, zipMethodPayload) {
			t.Fatalf("bzip2 fixture only covers zipMethodPayload: %v", err)
		}
		return raw
	case zipMethodZstd:
		w, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(payload)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	case zipMethodXZ:
		w, err := xz.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(payload)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

// zipWithRawMember writes one member whose body is already compressed, so a
// test can also store bytes that are not valid for the method.
func zipWithRawMember(t *testing.T, name string, method uint16, compressed []byte, size int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateRaw(&zip.FileHeader{
		Name:               name,
		Method:             method,
		CRC32:              crc32.ChecksumIEEE(zipMethodPayload),
		CompressedSize64:   uint64(len(compressed)),
		UncompressedSize64: uint64(size),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(compressed); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipWithMethod(t *testing.T, method uint16) []byte {
	t.Helper()
	return zipWithRawMember(t, "data.txt", method, compressForZip(t, method, zipMethodPayload), len(zipMethodPayload))
}

func TestZipMethodsOpenBytesExtract(t *testing.T) {
	for _, tc := range zipMethods {
		t.Run(tc.name, func(t *testing.T) {
			r, err := OpenBytes("test.zip", zipWithMethod(t, tc.method))
			if err != nil {
				t.Fatal(err)
			}
			rc, err := r.Extract("data.txt")
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, zipMethodPayload) {
				t.Fatalf("got %d bytes, want %d", len(got), len(zipMethodPayload))
			}
		})
	}
}

func readFirstStreamEntry(t *testing.T, stream *Stream, err error) []byte {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	entry, err := stream.Next()
	if err != nil {
		t.Fatal(err)
	}
	if entry.Path != "data.txt" {
		t.Fatalf("entry %q", entry.Path)
	}
	got, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestZipMethodsStreamAndRandomAccess(t *testing.T) {
	for _, tc := range zipMethods {
		t.Run(tc.name, func(t *testing.T) {
			data := zipWithMethod(t, tc.method)
			stream, err := OpenStreamBytes("test.zip", data, StreamOptions{})
			if got := readFirstStreamEntry(t, stream, err); !bytes.Equal(got, zipMethodPayload) {
				t.Fatalf("stream: got %d bytes", len(got))
			}
			stream, err = OpenStreamReaderAt("test.zip", bytes.NewReader(data), int64(len(data)), StreamOptions{})
			if got := readFirstStreamEntry(t, stream, err); !bytes.Equal(got, zipMethodPayload) {
				t.Fatalf("random access: got %d bytes", len(got))
			}
		})
	}
}

func TestZipMethodsMalformedDataIsAnError(t *testing.T) {
	garbage := bytes.Repeat([]byte{0xde, 0xad, 0xbe, 0xef}, 16)
	for _, tc := range zipMethods {
		t.Run(tc.name, func(t *testing.T) {
			data := zipWithRawMember(t, "data.txt", tc.method, garbage, len(zipMethodPayload))
			r, err := OpenBytes("test.zip", data)
			if err != nil {
				t.Fatal(err)
			}
			rc, err := r.Extract("data.txt")
			if err == nil {
				_, err = io.ReadAll(rc)
				_ = rc.Close()
			}
			if err == nil {
				t.Fatal("malformed data read without error")
			}
			if errors.Is(err, zip.ErrAlgorithm) {
				t.Fatalf("method still unsupported: %v", err)
			}

			stream, err := OpenStreamBytes("test.zip", data, StreamOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stream.Close() }()
			if _, err := stream.Next(); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadAll(stream); err == nil || errors.Is(err, zip.ErrAlgorithm) {
				t.Fatalf("stream read: %v", err)
			}
		})
	}
}

func TestZipMethodsStreamEntryLimit(t *testing.T) {
	for _, tc := range zipMethods {
		t.Run(tc.name, func(t *testing.T) {
			stream, err := OpenStreamBytes("test.zip", zipWithMethod(t, tc.method), StreamOptions{MaxEntryBytes: 100})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stream.Close() }()
			if _, err := stream.Next(); !errors.Is(err, ErrEntrySizeLimit) {
				t.Fatalf("Next: %v, want ErrEntrySizeLimit", err)
			}
		})
	}
}

func TestZipMethodsStreamCancellation(t *testing.T) {
	for _, tc := range zipMethods {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			stream, err := OpenStreamBytesContext(ctx, "test.zip", zipWithMethod(t, tc.method), StreamOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stream.Close() }()
			if _, err := stream.Next(); err != nil {
				t.Fatal(err)
			}
			cancel()
			if _, err := io.ReadAll(stream); !errors.Is(err, context.Canceled) {
				t.Fatalf("read after cancel: %v, want context.Canceled", err)
			}
		})
	}
}

func TestZipMethodsBrowser(t *testing.T) {
	for _, tc := range zipMethods {
		t.Run(tc.name, func(t *testing.T) {
			data := zipWithMethod(t, tc.method)
			reopen := func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
			b, err := OpenBrowser("test.zip", bytes.NewReader(data), reopen, BrowseOptions{})
			if err != nil {
				t.Fatal(err)
			}
			rc, err := b.Extract("data.txt")
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, zipMethodPayload) {
				t.Fatalf("got %d bytes, want %d", len(got), len(zipMethodPayload))
			}
		})
	}
}
