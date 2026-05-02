package archives

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	//nolint:gosec // verifying checksum output
	"crypto/md5"
	//nolint:gosec // verifying checksum output
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"testing"
)

func expectedDigests(data []byte) map[string]string {
	s1 := sha1.Sum(data) //nolint:gosec
	m5 := md5.Sum(data)  //nolint:gosec
	s256 := sha256.Sum256(data)
	s512 := sha512.Sum512(data)
	return map[string]string{
		SHA1:   hex.EncodeToString(s1[:]),
		MD5:    hex.EncodeToString(m5[:]),
		SHA256: hex.EncodeToString(s256[:]),
		SHA512: hex.EncodeToString(s512[:]),
	}
}

func createTestGem() []byte {
	var innerBuf bytes.Buffer
	innerGw := gzip.NewWriter(&innerBuf)
	innerTw := tar.NewWriter(innerGw)
	content := "puts 'hello'"
	_ = innerTw.WriteHeader(&tar.Header{
		Name: "lib/main.rb",
		Size: int64(len(content)),
		Mode: 0644,
	})
	_, _ = innerTw.Write([]byte(content))
	_ = innerTw.Close()
	_ = innerGw.Close()

	var gemBuf bytes.Buffer
	outerTw := tar.NewWriter(&gemBuf)
	dataTarGz := innerBuf.Bytes()
	_ = outerTw.WriteHeader(&tar.Header{
		Name: "data.tar.gz",
		Size: int64(len(dataTarGz)),
		Mode: 0644,
	})
	_, _ = outerTw.Write(dataTarGz)
	_ = outerTw.Close()
	return gemBuf.Bytes()
}

func TestHashAllAlgorithms(t *testing.T) {
	cases := []struct {
		name     string
		filename string
		data     []byte
	}{
		{"zip", "test.zip", createTestZip()},
		{"tar.gz", "test.tar.gz", createTestTarGz()},
		{"gem", "test.gem", createTestGem()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader, err := Open(tc.filename, bytes.NewReader(tc.data))
			if err != nil {
				t.Fatalf("Open failed: %v", err)
			}
			defer func() { _ = reader.Close() }()

			want := expectedDigests(tc.data)
			for algo, expected := range want {
				got, err := reader.Hash(algo)
				if err != nil {
					t.Fatalf("Hash(%s) failed: %v", algo, err)
				}
				if got != expected {
					t.Errorf("Hash(%s) = %s, want %s", algo, got, expected)
				}
			}
		})
	}
}

func TestHashCaseInsensitiveAlgo(t *testing.T) {
	data := createTestZip()
	reader, err := Open("test.zip", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer func() { _ = reader.Close() }()

	lower, _ := reader.Hash("sha256")
	upper, err := reader.Hash("SHA256")
	if err != nil {
		t.Fatalf("Hash(SHA256) failed: %v", err)
	}
	if lower != upper {
		t.Errorf("Hash should be case-insensitive: %s != %s", lower, upper)
	}
}

func TestHashUnsupportedAlgorithm(t *testing.T) {
	data := createTestZip()
	reader, err := Open("test.zip", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer func() { _ = reader.Close() }()

	_, err = reader.Hash("crc32")
	if err == nil {
		t.Fatal("expected error for unsupported algorithm")
	}
}

func TestHashThroughPrefixStripper(t *testing.T) {
	data := createTestTarGz()
	want := expectedDigests(data)[SHA256]

	reader, err := OpenWithPrefix("test.tgz", bytes.NewReader(data), "lib/")
	if err != nil {
		t.Fatalf("OpenWithPrefix failed: %v", err)
	}
	defer func() { _ = reader.Close() }()

	got, err := reader.Hash(SHA256)
	if err != nil {
		t.Fatalf("Hash failed: %v", err)
	}
	if got != want {
		t.Errorf("Hash through prefix stripper = %s, want %s", got, want)
	}
}

func TestHashGemIsOuterArchive(t *testing.T) {
	// The hash of a .gem must be of the outer tar, not the inner data.tar.gz,
	// since that is what registries publish checksums for.
	data := createTestGem()
	reader, err := openGem(data)
	if err != nil {
		t.Fatalf("openGem failed: %v", err)
	}
	defer func() { _ = reader.Close() }()

	want := expectedDigests(data)[SHA256]
	got, err := reader.Hash(SHA256)
	if err != nil {
		t.Fatalf("Hash failed: %v", err)
	}
	if got != want {
		t.Errorf("gem Hash = %s, want outer archive digest %s", got, want)
	}
}

func TestOpenBytes(t *testing.T) {
	data := createTestZip()
	reader, err := OpenBytes("test.zip", data)
	if err != nil {
		t.Fatalf("OpenBytes failed: %v", err)
	}
	defer func() { _ = reader.Close() }()

	got, err := reader.Hash(SHA256)
	if err != nil {
		t.Fatalf("Hash failed: %v", err)
	}
	want := expectedDigests(data)[SHA256]
	if got != want {
		t.Errorf("Hash = %s, want %s", got, want)
	}

	files, _ := reader.List()
	if len(files) != 4 {
		t.Errorf("List returned %d files, want 4", len(files))
	}
}
