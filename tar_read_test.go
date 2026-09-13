package archives

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

func tarWithPayloads(t testing.TB, payloads ...[]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for i, payload := range payloads {
		if err := tw.WriteHeader(&tar.Header{
			Name: fmt.Sprintf("package/lib/file%d", i),
			Size: int64(len(payload)),
			Mode: 0o644,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzipTar(t testing.TB, raw []byte) []byte {
	return compressTar(t, "test.tar.gz", raw)
}

func compressTar(t testing.TB, filename string, raw []byte) []byte {
	t.Helper()
	if filename == "test.tar" {
		return raw
	}
	var buf bytes.Buffer
	var w io.WriteCloser
	var err error
	switch filename {
	case "test.tar.gz":
		w = gzip.NewWriter(&buf)
	case "test.tar.zst":
		w, err = zstd.NewWriter(&buf)
	case "test.tar.xz":
		w, err = xz.NewWriter(&buf)
	default:
		t.Fatalf("unsupported test format: %s", filename)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestTarPayloadSizes(t *testing.T) {
	var payloads [][]byte
	for i, size := range []int{0, 1, 511, 512, 513, 16 << 10, (64 << 10) - 1, 64 << 10, (64 << 10) + 1, 1 << 20} {
		payloads = append(payloads, bytes.Repeat([]byte{byte(i)}, size))
	}
	raw := tarWithPayloads(t, payloads...)
	for _, filename := range []string{"test.tar", "test.tar.gz", "test.tar.zst", "test.tar.xz"} {
		t.Run(filename, func(t *testing.T) {
			data := compressTar(t, filename, raw)
			r, err := OpenBytesWithPrefix(filename, data, "package/")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			assertTarListing(t, r, payloads)
			gotHash, err := r.Hash(SHA256)
			if err != nil || gotHash != expectedDigests(data)[SHA256] {
				t.Fatalf("Hash = %q, %v", gotHash, err)
			}
			dir := t.TempDir()
			if err := ExtractAll(r, dir); err != nil {
				t.Fatal(err)
			}
			for i, payload := range payloads {
				assertFileContent(t, filepath.Join(dir, "lib", fmt.Sprintf("file%d", i)), string(payload))
			}
		})
	}
}

func assertTarListing(t *testing.T, r Reader, payloads [][]byte) {
	t.Helper()
	files, err := r.List()
	if err != nil || len(files) != len(payloads) {
		t.Fatalf("List: %d entries, %v", len(files), err)
	}
	dirFiles, err := r.ListDir("lib")
	if err != nil || !reflect.DeepEqual(files, dirFiles) {
		t.Fatalf("ListDir differs from List: %v", err)
	}
	for i := len(payloads) - 1; i >= 0; i-- {
		name := fmt.Sprintf("lib/file%d", i)
		if files[i].Path != name || files[i].Size != int64(len(payloads[i])) {
			t.Fatalf("unexpected metadata: %+v", files[i])
		}
		for range 2 {
			assertTarPayload(t, r, name, payloads[i])
		}
	}
}

func assertTarPayload(t testing.TB, r Reader, name string, want []byte) {
	t.Helper()
	src, err := r.Extract(name)
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(src)
	closeErr := src.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, want) {
		t.Fatalf("Extract(%q): got %d bytes, want %d; read %v, close %v", name, len(got), len(want), readErr, closeErr)
	}
}

func TestTarDuplicatePayloads(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	writeTarFile(t, tw, "package/index.js", "first", 0o644)
	writeTarFile(t, tw, "package/index.js", "second", 0o644)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := OpenWithPrefix("test.tar.gz", bytes.NewReader(gzipTar(t, buf.Bytes())), "package/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	files, err := r.List()
	if err != nil || len(files) != 2 || files[0].Size != 5 || files[1].Size != 6 {
		t.Fatalf("List = %+v, %v", files, err)
	}
	assertTarPayload(t, r, "index.js", []byte("first"))
	dir := t.TempDir()
	if err := ExtractAll(r, dir); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, filepath.Join(dir, "index.js"), "first")
}

func TestTarTruncatedPayload(t *testing.T) {
	for _, size := range []int64{1, 16 << 10, 1 << 20, 1 << 40} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			if err := tw.WriteHeader(&tar.Header{Name: "file", Size: size, Mode: 0o644}); err != nil {
				t.Fatal(err)
			}
			for _, filename := range []string{"test.tar", "test.tar.gz"} {
				_, err := OpenBytes(filename, compressTar(t, filename, buf.Bytes()))
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("%s: got %v, want unexpected EOF", filename, err)
				}
			}
		})
	}
}

func TestTarPayloadLimitBoundary(t *testing.T) {
	oldMax := maxDecompressedSize
	maxDecompressedSize = 1024
	t.Cleanup(func() { maxDecompressedSize = oldMax })
	for _, lastSize := range []int{511, 512, 513} {
		t.Run(fmt.Sprint(lastSize), func(t *testing.T) {
			raw := tarWithPayloads(t, make([]byte, 512), make([]byte, lastSize), nil)
			r, err := Open("test.tar.gz", bytes.NewReader(gzipTar(t, raw)))
			if lastSize > 512 {
				if !errors.Is(err, ErrDecompressLimit) {
					t.Fatalf("got %v, want decompression limit", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			assertTarPayload(t, r, "package/lib/file1", make([]byte, lastSize))
			assertTarPayload(t, r, "package/lib/file2", nil)
		})
	}
}

func TestTarPayloadReadErrors(t *testing.T) {
	for _, size := range []int{513, 16 << 10, 64 << 10, 1 << 20} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			raw := tarWithPayloads(t, make([]byte, size))
			_, err := OpenBytes("test.tar", raw[:512+size-1])
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("truncated payload: got %v, want unexpected EOF", err)
			}
			data := gzipTar(t, raw[:512+size])
			data[len(data)-8] ^= 1
			_, err = OpenBytes("test.tar.gz", data)
			if !errors.Is(err, gzip.ErrChecksum) {
				t.Fatalf("corrupt payload checksum: got %v, want checksum error", err)
			}
		})
	}
}

func FuzzTarPayloadRoundTrip(f *testing.F) {
	f.Add([]byte("module.exports = {};"))
	f.Add([]byte{})
	f.Add(bytes.Repeat([]byte("data"), 16385))
	f.Fuzz(func(t *testing.T, payload []byte) {
		if len(payload) > 1<<20 {
			t.Skip()
		}
		raw := tarWithPayloads(t, payload, []byte("last"))
		r, err := OpenBytes("test.tar.gz", gzipTar(t, raw))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Close() }()
		assertTarPayload(t, r, "package/lib/file0", payload)
		assertTarPayload(t, r, "package/lib/file1", []byte("last"))
	})
}
