package archives

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func writeTarZst(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(enc)
	for name, content := range files {
		_ = tw.WriteHeader(&tar.Header{
			Name: name,
			Size: int64(len(content)),
			Mode: 0644,
		})
		_, _ = tw.Write([]byte(content))
	}
	_ = tw.Close()
	_ = enc.Close()
	return buf.Bytes()
}

func createTestConda(t *testing.T) []byte {
	t.Helper()

	pkg := writeTarZst(t, map[string]string{
		"site-packages/six.py":       "print('six')",
		"info/licenses/LICENSE":      "MIT",
		"site-packages/six-1.0/META": "meta",
	})
	info := writeTarZst(t, map[string]string{
		"info/index.json": `{"name":"six"}`,
		"info/paths.json": `{"paths":[]}`,
	})

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, m := range []struct {
		name string
		data []byte
	}{
		{"metadata.json", []byte(`{"conda_pkg_format_version": 2}`)},
		{"pkg-six-1.0-py_0.tar.zst", pkg},
		{"info-six-1.0-py_0.tar.zst", info},
	} {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: m.name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(m.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCondaReader(t *testing.T) {
	data := createTestConda(t)
	reader, err := Open("six-1.0-py_0.conda", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Open conda failed: %v", err)
	}
	defer func() { _ = reader.Close() }()

	if _, ok := reader.(*tarReader); !ok {
		t.Fatalf("reader = %T, want *tarReader", reader)
	}

	files, err := reader.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(files) != 5 {
		t.Errorf("List returned %d files, want 5", len(files))
	}

	// Extract from the pkg tarball
	rc, err := reader.Extract("site-packages/six.py")
	if err != nil {
		t.Fatalf("Extract pkg file failed: %v", err)
	}
	content, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(content) != "print('six')" {
		t.Errorf("pkg content = %q", string(content))
	}

	// Extract from the info tarball
	rc, err = reader.Extract("info/index.json")
	if err != nil {
		t.Fatalf("Extract info file failed: %v", err)
	}
	content, _ = io.ReadAll(rc)
	_ = rc.Close()
	if string(content) != `{"name":"six"}` {
		t.Errorf("info content = %q", string(content))
	}

	// ListDir should merge both tarballs: root has site-packages/ and info/
	root, err := reader.ListDir("")
	if err != nil {
		t.Fatalf("ListDir failed: %v", err)
	}
	assertNoDuplicates(t, "conda ListDir root", root)
	dirs := map[string]bool{}
	for _, f := range root {
		if f.IsDir {
			dirs[f.Name] = true
		}
	}
	if !dirs["site-packages"] || !dirs["info"] {
		t.Errorf("ListDir root dirs = %v, want site-packages and info", dirs)
	}
}

func TestCondaHashIsOuterArchive(t *testing.T) {
	data := createTestConda(t)
	reader, err := openConda(data)
	if err != nil {
		t.Fatalf("openConda failed: %v", err)
	}
	defer func() { _ = reader.Close() }()

	want := expectedDigests(data)[SHA256]
	got, err := reader.Hash(SHA256)
	if err != nil {
		t.Fatalf("Hash failed: %v", err)
	}
	if got != want {
		t.Errorf("conda Hash = %s, want outer archive digest %s", got, want)
	}
}

func TestOpenCondaMissingMembers(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("metadata.json")
	_, _ = w.Write([]byte(`{"conda_pkg_format_version": 2}`))
	_ = zw.Close()

	_, err := openConda(buf.Bytes())
	if err == nil {
		t.Fatal("conda zip with no tar.zst members was accepted")
	}
}

func TestOpenCondaRejectsCumulativeOverflow(t *testing.T) {
	oldMax := maxDecompressedSize
	maxDecompressedSize = 1024
	defer func() { maxDecompressedSize = oldMax }()

	// Each member decompresses to 768 bytes, under the 1024 limit on its
	// own; together they exceed it.
	member := writeTarZst(t, map[string]string{"blob": strings.Repeat("x", 768)})

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"pkg-a-1.tar.zst", "info-a-1.tar.zst"} {
		w, _ := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		_, _ = w.Write(member)
	}
	_ = zw.Close()

	_, err := openConda(buf.Bytes())
	if err == nil {
		t.Fatal("expected error when cumulative decompressed size exceeds limit")
	}
	if !errors.Is(err, ErrDecompressLimit) {
		t.Fatalf("expected ErrDecompressLimit, got: %v", err)
	}
}

func TestOpenDoesNotInferConda(t *testing.T) {
	reader, err := OpenBytes("artifact", createTestConda(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	if _, ok := reader.(*zipReader); !ok {
		t.Fatalf("reader = %T, want generic ZIP reader", reader)
	}
}
