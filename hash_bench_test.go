package archives

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"math/rand"
	"testing"
)

func createBenchTarGz(fileCount, fileSize int) []byte {
	rnd := rand.New(rand.NewSource(1)) //nolint:gosec
	buf := new(bytes.Buffer)
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)

	payload := make([]byte, fileSize)
	for i := range fileCount {
		rnd.Read(payload)
		_ = tw.WriteHeader(&tar.Header{
			Name: fmt.Sprintf("lib/file%04d.dat", i),
			Size: int64(fileSize),
			Mode: 0644,
		})
		_, _ = tw.Write(payload)
	}
	_ = tw.Close()
	_ = gw.Close()
	return buf.Bytes()
}

// ~1 MiB compressed, comparable to a mid-sized npm/gem tarball.
var benchArchive = createBenchTarGz(64, 16*1024)

func BenchmarkHash(b *testing.B) {
	reader, err := OpenBytes("bench.tar.gz", benchArchive)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = reader.Close() })

	for _, algo := range []string{SHA256, SHA512, SHA1, MD5} {
		b.Run(algo, func(b *testing.B) {
			b.SetBytes(int64(len(benchArchive)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := reader.Hash(algo); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkOpenTarGz(b *testing.B) {
	b.SetBytes(int64(len(benchArchive)))
	b.ReportAllocs()
	for b.Loop() {
		r, err := Open("bench.tar.gz", bytes.NewReader(benchArchive))
		if err != nil {
			b.Fatal(err)
		}
		_ = r.Close()
	}
}

func BenchmarkOpenBytesTarGz(b *testing.B) {
	b.SetBytes(int64(len(benchArchive)))
	b.ReportAllocs()
	for b.Loop() {
		r, err := OpenBytes("bench.tar.gz", benchArchive)
		if err != nil {
			b.Fatal(err)
		}
		_ = r.Close()
	}
}
