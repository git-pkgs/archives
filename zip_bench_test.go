package archives

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"testing"
)

// createBenchZip builds a zip of fileCount deflated members. Payload bytes
// are drawn from a 4-symbol alphabet giving a ~34% deflate ratio, so the
// decoder does real Huffman and match-copy work; fully random data would
// be emitted as stored blocks and skip the decoder.
func createBenchZip(fileCount, fileSize int) []byte {
	rnd := rand.New(rand.NewSource(1)) //nolint:gosec
	payload := make([]byte, fileSize)
	buf := new(bytes.Buffer)
	w := zip.NewWriter(buf)
	for i := range fileCount {
		for j := range payload {
			payload[j] = byte(rnd.Intn(4))
		}
		f, _ := w.CreateHeader(&zip.FileHeader{
			Name:   fmt.Sprintf("lib/file%04d.dat", i),
			Method: zip.Deflate,
		})
		_, _ = f.Write(payload)
	}
	_ = w.Close()
	return buf.Bytes()
}

const (
	benchZipFileCount = 64
	benchZipFileSize  = 16 * 1024
)

var benchZipArchive = createBenchZip(benchZipFileCount, benchZipFileSize)

// BenchmarkZipExtract measures Open + draining every deflated member.
// zip decompresses lazily on Extract, so this is where the flate decoder
// cost lands; ExtractAll adds filesystem noise on top of the same loop.
func BenchmarkZipExtract(b *testing.B) {
	b.SetBytes(int64(benchZipFileCount * benchZipFileSize))
	b.ReportAllocs()
	for b.Loop() {
		r, err := OpenBytes("bench.zip", benchZipArchive)
		if err != nil {
			b.Fatal(err)
		}
		files, err := r.List()
		if err != nil {
			b.Fatal(err)
		}
		for _, f := range files {
			src, err := r.Extract(f.Path)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := io.Copy(io.Discard, src); err != nil {
				b.Fatal(err)
			}
			if err := src.Close(); err != nil {
				b.Fatal(err)
			}
		}
		if err := r.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
