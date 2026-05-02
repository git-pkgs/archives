package archives

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDetectFormat(t *testing.T) {
	tests := []struct {
		filename string
		expected string
	}{
		{"package.zip", "zip"},
		{"package.jar", "zip"},
		{"package.whl", "zip"},
		{"package.nupkg", "zip"},
		{"package.tar", "tar"},
		{"package.tar.gz", "tar.gz"},
		{"package.tgz", "tgz"},
		{"package.tar.bz2", "tar.bz2"},
		{"package.tar.xz", "tar.xz"},
		{"package.gem", "gem"},
		{"unknown.txt", ""},
		{"Package.ZIP", "zip"}, // Case insensitive
		{"package.TAR.GZ", "tar.gz"},
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			got := detectFormat(tt.filename)
			if got != tt.expected {
				t.Errorf("detectFormat(%q) = %q, want %q", tt.filename, got, tt.expected)
			}
		})
	}
}

func TestNormalizeDir(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", ""},
		{"/", ""},
		{"dir", "dir/"},
		{"dir/", "dir/"},
		{"/dir/", "dir/"},
		{"  dir  ", "dir/"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := normalizeDir(tt.input)
			if got != tt.expected {
				t.Errorf("normalizeDir(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestIsInDir(t *testing.T) {
	tests := []struct {
		filePath string
		dirPath  string
		expected bool
	}{
		{"file.txt", "", true},
		{"dir/file.txt", "", false},
		{"dir/file.txt", "dir", true},
		{"dir/subdir/file.txt", "dir", false},
		{"dir/subdir/file.txt", "dir/subdir", true},
		{"other/file.txt", "dir", false},
		{"dir/", "", true}, // dir entry is in root
	}

	for _, tt := range tests {
		t.Run(tt.filePath+"_in_"+tt.dirPath, func(t *testing.T) {
			got := isInDir(tt.filePath, tt.dirPath)
			if got != tt.expected {
				t.Errorf("isInDir(%q, %q) = %v, want %v", tt.filePath, tt.dirPath, got, tt.expected)
			}
		})
	}
}

func TestExtractName(t *testing.T) {
	tests := []struct {
		path     string
		expected string
	}{
		{"file.txt", "file.txt"},
		{"dir/file.txt", "file.txt"},
		{"dir/subdir/file.txt", "file.txt"},
		{"dir/", "dir"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := extractName(tt.path)
			if got != tt.expected {
				t.Errorf("extractName(%q) = %q, want %q", tt.path, got, tt.expected)
			}
		})
	}
}

// createTestZip creates a zip archive in memory with test files
func createTestZip() []byte {
	buf := new(bytes.Buffer)
	w := zip.NewWriter(buf)

	files := []struct {
		name    string
		content string
	}{
		{"README.md", "# Test Package"},
		{"src/main.go", "package main"},
		{"src/util/helper.go", "package util"},
		{"docs/guide.md", "# Guide"},
	}

	for _, file := range files {
		f, _ := w.Create(file.name)
		_, _ = f.Write([]byte(file.content))
	}

	_ = w.Close()
	return buf.Bytes()
}

func TestZipReader(t *testing.T) {
	data := createTestZip()
	reader, err := openZip(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("openZip failed: %v", err)
	}
	defer func() { _ = reader.Close() }()

	// Test List
	files, err := reader.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(files) != 4 {
		t.Errorf("List returned %d files, want 4", len(files))
	}

	// Test ListDir root
	rootFiles, err := reader.ListDir("")
	if err != nil {
		t.Fatalf("ListDir failed: %v", err)
	}
	if len(rootFiles) < 2 {
		t.Errorf("ListDir root returned %d items, want at least 2", len(rootFiles))
	}

	// Test Extract
	rc, err := reader.Extract("README.md")
	if err != nil {
		t.Fatalf("Extract failed: %v", err)
	}
	defer func() { _ = rc.Close() }()

	content, _ := io.ReadAll(rc)
	if string(content) != "# Test Package" {
		t.Errorf("Extract content = %q, want %q", string(content), "# Test Package")
	}

	// Test Extract non-existent file
	_, err = reader.Extract("nonexistent.txt")
	if err == nil {
		t.Error("Extract non-existent file should fail")
	}
}

// createTestTarGz creates a tar.gz archive in memory with test files
func createTestTarGz() []byte {
	buf := new(bytes.Buffer)
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)

	files := []struct {
		name    string
		content string
	}{
		{"package.json", `{"name": "test"}`},
		{"index.js", "console.log('hello');"},
		{"lib/util.js", "module.exports = {};"},
	}

	for _, file := range files {
		header := &tar.Header{
			Name:    file.name,
			Size:    int64(len(file.content)),
			Mode:    0644,
			ModTime: time.Now(),
		}
		_ = tw.WriteHeader(header)
		_, _ = tw.Write([]byte(file.content))
	}

	_ = tw.Close()
	_ = gw.Close()
	return buf.Bytes()
}

func TestTarReader(t *testing.T) {
	data := createTestTarGz()
	reader, err := openTar(bytes.NewReader(data), "gzip")
	if err != nil {
		t.Fatalf("openTar failed: %v", err)
	}
	defer func() { _ = reader.Close() }()

	// Test List
	files, err := reader.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(files) != 3 {
		t.Errorf("List returned %d files, want 3", len(files))
	}

	// Test ListDir
	rootFiles, err := reader.ListDir("")
	if err != nil {
		t.Fatalf("ListDir failed: %v", err)
	}
	if len(rootFiles) < 2 {
		t.Errorf("ListDir root returned %d items, want at least 2", len(rootFiles))
	}

	// Test Extract
	rc, err := reader.Extract("package.json")
	if err != nil {
		t.Fatalf("Extract failed: %v", err)
	}
	defer func() { _ = rc.Close() }()

	content, _ := io.ReadAll(rc)
	if !strings.Contains(string(content), "test") {
		t.Errorf("Extract content doesn't contain expected data")
	}
}

func TestOpen(t *testing.T) {
	// Test ZIP
	zipData := createTestZip()
	reader, err := Open("test.zip", bytes.NewReader(zipData))
	if err != nil {
		t.Fatalf("Open zip failed: %v", err)
	}
	_ = reader.Close()

	// Test TAR.GZ
	tgzData := createTestTarGz()
	reader, err = Open("test.tar.gz", bytes.NewReader(tgzData))
	if err != nil {
		t.Fatalf("Open tar.gz failed: %v", err)
	}
	_ = reader.Close()

	// Test unsupported format
	_, err = Open("test.unknown", bytes.NewReader([]byte("data")))
	if err == nil {
		t.Error("Open with unsupported format should fail")
	}
}

func TestZipListDir(t *testing.T) {
	data := createTestZip()
	reader, err := openZip(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("openZip failed: %v", err)
	}
	defer func() { _ = reader.Close() }()

	// Test listing src directory
	srcFiles, err := reader.ListDir("src")
	if err != nil {
		t.Fatalf("ListDir src failed: %v", err)
	}

	// Should have main.go and util/ subdirectory
	if len(srcFiles) != 2 {
		t.Errorf("ListDir src returned %d items, want 2", len(srcFiles))
	}

	// Check that we have both a file and a directory
	hasFile := false
	hasDir := false
	for _, f := range srcFiles {
		if f.Name == "main.go" && !f.IsDir {
			hasFile = true
		}
		if f.Name == "util" && f.IsDir {
			hasDir = true
		}
	}

	if !hasFile {
		t.Error("ListDir src should include main.go file")
	}
	if !hasDir {
		t.Error("ListDir src should include util directory")
	}
}

func TestOpenWithPrefix(t *testing.T) {
	// Create archive with package/ prefix (like npm)
	buf := new(bytes.Buffer)
	w := zip.NewWriter(buf)

	files := []struct {
		name    string
		content string
	}{
		{"package/README.md", "# Test"},
		{"package/index.js", "console.log('test');"},
		{"package/lib/util.js", "module.exports = {};"},
	}

	for _, file := range files {
		f, _ := w.Create(file.name)
		_, _ = f.Write([]byte(file.content))
	}
	_ = w.Close()

	// Open with prefix stripping
	reader, err := OpenWithPrefix("test.zip", bytes.NewReader(buf.Bytes()), "package/")
	if err != nil {
		t.Fatalf("OpenWithPrefix failed: %v", err)
	}
	defer func() { _ = reader.Close() }()

	// List files - should not include package/ prefix
	files2, err := reader.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	// Check that package/ prefix is stripped
	for _, f := range files2 {
		if strings.HasPrefix(f.Path, "package/") {
			t.Errorf("path %q still has package/ prefix", f.Path)
		}
	}

	// Should have 3 files without the prefix
	if len(files2) != 3 {
		t.Errorf("List returned %d files, want 3", len(files2))
	}

	// Test ListDir root
	rootFiles, err := reader.ListDir("")
	if err != nil {
		t.Fatalf("ListDir failed: %v", err)
	}

	// Should see README.md and index.js in root, plus lib/ directory
	if len(rootFiles) < 2 {
		t.Errorf("ListDir root returned %d items, want at least 2", len(rootFiles))
	}

	// Test Extract
	rc, err := reader.Extract("README.md")
	if err != nil {
		t.Fatalf("Extract failed: %v", err)
	}
	defer func() { _ = rc.Close() }()

	content, _ := io.ReadAll(rc)
	if string(content) != "# Test" {
		t.Errorf("Extract content = %q, want %q", string(content), "# Test")
	}
}

// createTestZipWithDirEntries creates a zip with explicit directory entries,
// like GitHub zipball downloads produce.
func createTestZipWithDirEntries() []byte {
	buf := new(bytes.Buffer)
	w := zip.NewWriter(buf)

	// Add explicit directory entry (GitHub zipballs do this)
	header := &zip.FileHeader{
		Name:     "project-abc123/",
		Method:   zip.Store,
		Modified: time.Date(2026, 3, 30, 8, 14, 47, 0, time.UTC),
	}
	header.SetMode(0755 | os.ModeDir)
	_, _ = w.CreateHeader(header)

	// Add explicit src/ subdirectory entry
	srcHeader := &zip.FileHeader{
		Name:     "project-abc123/src/",
		Method:   zip.Store,
		Modified: time.Date(2026, 3, 30, 8, 14, 47, 0, time.UTC),
	}
	srcHeader.SetMode(0755 | os.ModeDir)
	_, _ = w.CreateHeader(srcHeader)

	// Add files inside that directory
	files := []struct {
		name    string
		content string
	}{
		{"project-abc123/README.md", "# Test"},
		{"project-abc123/src/main.go", "package main"},
		{"project-abc123/src/util.go", "package main"},
	}

	for _, file := range files {
		f, _ := w.Create(file.name)
		_, _ = f.Write([]byte(file.content))
	}

	_ = w.Close()
	return buf.Bytes()
}

// createTestTarGzWithDirEntries creates a tar.gz with explicit directory entries,
// like GitHub tarball downloads produce.
func createTestTarGzWithDirEntries() []byte {
	buf := new(bytes.Buffer)
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)

	// Add explicit directory entries
	for _, dir := range []string{"project-abc123/", "project-abc123/src/"} {
		_ = tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir,
			Name:     dir,
			Mode:     0755,
			ModTime:  time.Date(2026, 3, 30, 8, 14, 47, 0, time.UTC),
		})
	}

	files := []struct {
		name    string
		content string
	}{
		{"project-abc123/README.md", "# Test"},
		{"project-abc123/src/main.go", "package main"},
		{"project-abc123/src/util.go", "package main"},
	}

	for _, file := range files {
		_ = tw.WriteHeader(&tar.Header{
			Name:    file.name,
			Size:    int64(len(file.content)),
			Mode:    0644,
			ModTime: time.Date(2026, 3, 30, 8, 14, 47, 0, time.UTC),
		})
		_, _ = tw.Write([]byte(file.content))
	}

	_ = tw.Close()
	_ = gw.Close()
	return buf.Bytes()
}

func assertNoDuplicates(t *testing.T, label string, files []FileInfo) {
	t.Helper()
	seen := map[string]int{}
	for _, f := range files {
		seen[f.Path]++
	}
	for path, count := range seen {
		if count > 1 {
			t.Errorf("%s: %d entries for %q, want 1", label, count, path)
		}
	}
}

func TestZipListDirNoDuplicatesWithExplicitDirEntries(t *testing.T) {
	data := createTestZipWithDirEntries()
	reader, err := openZip(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("openZip failed: %v", err)
	}
	defer func() { _ = reader.Close() }()

	files, err := reader.ListDir("")
	if err != nil {
		t.Fatalf("ListDir root failed: %v", err)
	}
	assertNoDuplicates(t, "ListDir root", files)

	files, err = reader.ListDir("project-abc123/")
	if err != nil {
		t.Fatalf("ListDir subdir failed: %v", err)
	}
	assertNoDuplicates(t, "ListDir project-abc123/", files)
}

func TestTarListDirNoDuplicatesWithExplicitDirEntries(t *testing.T) {
	data := createTestTarGzWithDirEntries()
	reader, err := openTar(bytes.NewReader(data), "gzip")
	if err != nil {
		t.Fatalf("openTar failed: %v", err)
	}
	defer func() { _ = reader.Close() }()

	files, err := reader.ListDir("")
	if err != nil {
		t.Fatalf("ListDir root failed: %v", err)
	}
	assertNoDuplicates(t, "ListDir root", files)

	files, err = reader.ListDir("project-abc123/")
	if err != nil {
		t.Fatalf("ListDir subdir failed: %v", err)
	}
	assertNoDuplicates(t, "ListDir project-abc123/", files)
}

func TestGetStripPrefixNpm(t *testing.T) {
	// Create npm-style archive
	buf := new(bytes.Buffer)
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)

	files := map[string]string{
		"package/package.json": `{"name": "test"}`,
		"package/index.js":     "console.log('test');",
	}

	for path, content := range files {
		header := &tar.Header{
			Name: path,
			Size: int64(len(content)),
			Mode: 0644,
		}
		_ = tw.WriteHeader(header)
		_, _ = tw.Write([]byte(content))
	}
	_ = tw.Close()
	_ = gw.Close()

	// Open with npm prefix stripping
	reader, err := OpenWithPrefix("test.tgz", bytes.NewReader(buf.Bytes()), "package/")
	if err != nil {
		t.Fatalf("OpenWithPrefix failed: %v", err)
	}
	defer func() { _ = reader.Close() }()

	// List root - should see package.json and index.js directly
	rootFiles, err := reader.ListDir("")
	if err != nil {
		t.Fatalf("ListDir failed: %v", err)
	}

	if len(rootFiles) != 2 {
		t.Errorf("expected 2 files in root, got %d", len(rootFiles))
	}

	// Verify files are at root level
	for _, f := range rootFiles {
		if strings.Contains(f.Path, "/") {
			t.Errorf("file %q should be at root level after prefix stripping", f.Path)
		}
	}
}

func TestOpenTarRejectsDecompressBomb(t *testing.T) {
	oldMax := maxDecompressedSize
	maxDecompressedSize = 1024
	defer func() { maxDecompressedSize = oldMax }()

	buf := new(bytes.Buffer)
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)

	content := strings.Repeat("x", 2048)
	_ = tw.WriteHeader(&tar.Header{
		Name: "big.txt",
		Size: int64(len(content)),
		Mode: 0644,
	})
	_, _ = tw.Write([]byte(content))
	_ = tw.Close()
	_ = gw.Close()

	_, err := openTar(bytes.NewReader(buf.Bytes()), "gzip")
	if err == nil {
		t.Fatal("expected error for oversized decompressed content")
	}
	if !errors.Is(err, ErrDecompressLimit) {
		t.Fatalf("expected ErrDecompressLimit, got: %v", err)
	}
}

func TestOpenTarAcceptsWithinLimit(t *testing.T) {
	oldMax := maxDecompressedSize
	maxDecompressedSize = 4096
	defer func() { maxDecompressedSize = oldMax }()

	buf := new(bytes.Buffer)
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)

	content := strings.Repeat("x", 1024)
	_ = tw.WriteHeader(&tar.Header{
		Name: "ok.txt",
		Size: int64(len(content)),
		Mode: 0644,
	})
	_, _ = tw.Write([]byte(content))
	_ = tw.Close()
	_ = gw.Close()

	reader, err := openTar(bytes.NewReader(buf.Bytes()), "gzip")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	files, _ := reader.List()
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0].Path != "ok.txt" {
		t.Errorf("expected ok.txt, got %s", files[0].Path)
	}
}

func TestOpenTarRejectsCumulativeOverflow(t *testing.T) {
	oldMax := maxDecompressedSize
	maxDecompressedSize = 1024
	defer func() { maxDecompressedSize = oldMax }()

	buf := new(bytes.Buffer)
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)

	for i := 0; i < 3; i++ {
		content := strings.Repeat("y", 512)
		_ = tw.WriteHeader(&tar.Header{
			Name: fmt.Sprintf("file%d.txt", i),
			Size: int64(len(content)),
			Mode: 0644,
		})
		_, _ = tw.Write([]byte(content))
	}
	_ = tw.Close()
	_ = gw.Close()

	_, err := openTar(bytes.NewReader(buf.Bytes()), "gzip")
	if err == nil {
		t.Fatal("expected error when cumulative size exceeds limit")
	}
	if !errors.Is(err, ErrDecompressLimit) {
		t.Fatalf("expected ErrDecompressLimit, got: %v", err)
	}
}

func TestOpenGemRejectsOversizedData(t *testing.T) {
	oldMax := maxDecompressedSize
	maxDecompressedSize = 512
	defer func() { maxDecompressedSize = oldMax }()

	// Build a data.tar.gz that decompresses larger than the limit
	var innerBuf bytes.Buffer
	innerGw := gzip.NewWriter(&innerBuf)
	innerTw := tar.NewWriter(innerGw)

	content := strings.Repeat("z", 1024)
	_ = innerTw.WriteHeader(&tar.Header{
		Name: "lib/main.rb",
		Size: int64(len(content)),
		Mode: 0644,
	})
	_, _ = innerTw.Write([]byte(content))
	_ = innerTw.Close()
	_ = innerGw.Close()

	// Wrap in outer gem tar
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

	_, err := openGem(bytes.NewReader(gemBuf.Bytes()))
	if err == nil {
		t.Fatal("expected error for oversized gem data")
	}
}
