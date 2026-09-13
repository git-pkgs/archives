package archives

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"testing"
)

func BenchmarkTarBrowse(b *testing.B) {
	for _, size := range []int{0, 512, 16 << 10, 64 << 10, 1 << 20} {
		payload := make([]byte, size)
		for i := range payload {
			payload[i] = byte(i)
		}
		for _, filename := range []string{"test.tar", "test.tar.gz"} {
			b.Run(fmt.Sprintf("%s/%d", filename, size), func(b *testing.B) {
				benchmarkTarBrowse(b, filename, payload)
			})
		}
	}
}

func BenchmarkListDir(b *testing.B) {
	const (
		files    = 2000
		subdirs  = 20
		filePerm = 0o644
	)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for i := range files {
		_ = tw.WriteHeader(&tar.Header{
			Name: fmt.Sprintf("package/lib/sub%02d/file%04d.dat", i%subdirs, i),
			Mode: filePerm,
		})
	}
	_ = tw.Close()
	r, err := OpenBytesWithPrefix("test.tar", buf.Bytes(), "package/")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = r.Close() })

	for _, dir := range []string{"", "lib", "lib/sub00"} {
		b.Run(dir, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := r.ListDir(dir); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func benchmarkTarBrowse(b *testing.B, filename string, payload []byte) {
	b.Helper()
	raw := compressTar(b, filename, tarWithPayloads(b, payload, payload, payload, payload))
	b.ReportAllocs()
	b.SetBytes(int64(4 * len(payload)))
	for b.Loop() {
		r, err := OpenBytesWithPrefix(filename, raw, "package/")
		if err != nil {
			b.Fatal(err)
		}
		files, err := r.ListDir("lib")
		if err != nil || len(files) != 4 {
			b.Fatalf("ListDir: %d files, %v", len(files), err)
		}
		src, err := r.Extract("lib/file3")
		if err != nil {
			b.Fatal(err)
		}
		n, err := io.Copy(io.Discard, src)
		if err != nil || n != int64(len(payload)) {
			b.Fatalf("Extract: %d bytes, %v", n, err)
		}
		if err := src.Close(); err != nil {
			b.Fatal(err)
		}
		if err := r.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
