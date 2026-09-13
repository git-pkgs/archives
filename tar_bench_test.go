package archives

import (
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
