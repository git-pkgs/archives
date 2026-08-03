package archives

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestExtractAllZip(t *testing.T) {
	reader, err := OpenBytes("test.zip", createTestZip())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	dir := t.TempDir()
	if err := ExtractAll(reader, dir); err != nil {
		t.Fatal(err)
	}

	assertFileContent(t, filepath.Join(dir, "README.md"), "# Test Package")
	assertFileContent(t, filepath.Join(dir, "src", "main.go"), "package main")
	assertFileContent(t, filepath.Join(dir, "src", "util", "helper.go"), "package util")
	assertFileContent(t, filepath.Join(dir, "docs", "guide.md"), "# Guide")
}

func TestExtractAllTarGz(t *testing.T) {
	reader, err := OpenBytes("test.tgz", createTestTarGz())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	dir := t.TempDir()
	if err := ExtractAll(reader, dir); err != nil {
		t.Fatal(err)
	}

	assertFileContent(t, filepath.Join(dir, "package.json"), `{"name": "test"}`)
	assertFileContent(t, filepath.Join(dir, "lib", "util.js"), "module.exports = {};")
}

func TestExtractAllCreatesTargetDir(t *testing.T) {
	reader, err := OpenBytes("test.zip", createTestZip())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	dir := filepath.Join(t.TempDir(), "sub", "target")
	if err := ExtractAll(reader, dir); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, filepath.Join(dir, "README.md"), "# Test Package")
}

func TestExtractAllPreservesTarMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not preserved on windows")
	}

	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)
	if err := tw.WriteHeader(&tar.Header{Name: "conf/", Typeflag: tar.TypeDir, Mode: 0o550}); err != nil {
		t.Fatal(err)
	}
	writeTarFile(t, tw, "conf/settings", "x", 0o640)
	writeTarFile(t, tw, "bin/tool", "#!/bin/sh\n", 0o755)
	writeTarFile(t, tw, "data.txt", "hello", 0o600)
	_ = tw.Close()

	reader, err := OpenBytes("test.tar", buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	dir := t.TempDir()
	if err := ExtractAll(reader, dir); err != nil {
		t.Fatal(err)
	}

	assertFileMode(t, filepath.Join(dir, "bin", "tool"), 0o755)
	assertFileMode(t, filepath.Join(dir, "data.txt"), 0o600)
	assertFileMode(t, filepath.Join(dir, "conf", "settings"), 0o640)
	// Directory mode is applied after its contents are written and is not
	// widened for owner access.
	assertFileMode(t, filepath.Join(dir, "conf"), 0o550)

	// Restore write so t.TempDir cleanup can remove it.
	_ = os.Chmod(filepath.Join(dir, "conf"), 0o750)
}

func TestExtractAllPreservesZeroMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not preserved on windows")
	}

	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)
	writeTarFile(t, tw, "locked", "x", 0o000)
	_ = tw.Close()

	reader, err := OpenBytes("test.tar", buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	dir := t.TempDir()
	if err := ExtractAll(reader, dir); err != nil {
		t.Fatal(err)
	}
	assertFileMode(t, filepath.Join(dir, "locked"), 0o000)
}

func TestExtractAllZipWithoutUnixModeUsesDefault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not preserved on windows")
	}

	// createTestZip writes entries via zip.Writer.Create, which does not
	// set ExternalAttrs, so no Unix mode is recorded.
	reader, err := OpenBytes("test.zip", createTestZip())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	dir := t.TempDir()
	if err := ExtractAll(reader, dir); err != nil {
		t.Fatal(err)
	}
	// The exact mode depends on the process umask; the check is that the
	// synthesised 0666 was not applied via Chmod.
	info, err := os.Stat(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		t.Fatalf("file mode = %o, want group/other write cleared", perm)
	}
}

func TestZipHasUnixMode(t *testing.T) {
	tests := []struct {
		name    string
		creator uint16
		attrs   uint32
		want    bool
	}{
		{"Unix with mode", zipCreatorUnix, 0o100644 << zipUnixModeShift, true},
		{"macOS with mode", zipCreatorMacOSX, 0o100644 << zipUnixModeShift, true},
		{"Unix no mode", zipCreatorUnix, 0, false},
		{"FAT with high bits", 0, 0xffff0020, false},
		{"NTFS with high bits", 11, 0x00010020, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &zip.FileHeader{
				CreatorVersion: tt.creator << zipCreatorShift,
				ExternalAttrs:  tt.attrs,
			}
			if got := zipHasUnixMode(h); got != tt.want {
				t.Fatalf("zipHasUnixMode(creator=%d, attrs=%#x) = %v, want %v",
					tt.creator, tt.attrs, got, tt.want)
			}
		})
	}
}

func TestExtractAllRejectsTraversal(t *testing.T) {
	tests := []struct {
		name  string
		entry string
	}{
		{"parent", "../escape.txt"},
		{"nested parent", "sub/../../escape.txt"},
		{"absolute", "/etc/passwd"},
	}
	if runtime.GOOS == "windows" {
		tests = append(tests,
			struct{ name, entry string }{"drive", `C:\escape.txt`},
			struct{ name, entry string }{"backslash parent", `..\escape.txt`},
		)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader, err := OpenBytes("test.tar", createTarWithEntry(t, tt.entry))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reader.Close() }()

			dir := t.TempDir()
			err = ExtractAll(reader, dir)
			if !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("ExtractAll(%q) error = %v, want ErrUnsafePath", tt.entry, err)
			}
			if !strings.Contains(err.Error(), tt.entry) {
				t.Fatalf("error %q does not name offending entry %q", err, tt.entry)
			}
			assertNoEscape(t, dir)
		})
	}
}

func TestExtractAllAcceptsBenignDotSegments(t *testing.T) {
	tests := []struct {
		entry string
		want  string
	}{
		{"./file.txt", "file.txt"},
		{"sub/./file.txt", filepath.Join("sub", "file.txt")},
		{"sub/inner/../file.txt", filepath.Join("sub", "file.txt")},
	}

	for _, tt := range tests {
		t.Run(tt.entry, func(t *testing.T) {
			reader, err := OpenBytes("test.tar", createTarWithEntry(t, tt.entry))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reader.Close() }()

			dir := t.TempDir()
			if err := ExtractAll(reader, dir); err != nil {
				t.Fatalf("ExtractAll(%q) failed: %v", tt.entry, err)
			}
			assertFileContent(t, filepath.Join(dir, tt.want), "x")
		})
	}
}

func TestExtractAllConfinedByRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Symlink requires elevated privileges on windows")
	}

	outside := t.TempDir()
	dir := t.TempDir()
	// Plant a symlink under the target that points outside it. os.Root must
	// refuse to follow it when writing sub/file.
	if err := os.Symlink(outside, filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenBytes("test.tar", createTarWithEntry(t, "sub/file"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	if err := ExtractAll(reader, dir); err == nil {
		t.Fatal("write through escaping symlink succeeded")
	}

	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatalf("write escaped to %s: %v", outside, entries)
	}
}

func TestExtractAllReplacesExistingObjects(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Symlink requires elevated privileges on windows")
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("original"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A pre-existing in-root symlink at the destination is replaced with a
	// fresh regular file rather than followed.
	if err := os.Symlink("target", filepath.Join(dir, "package.json")); err != nil {
		t.Fatal(err)
	}
	// A pre-existing hard link at the destination is unlinked; the sibling
	// name keeps the original inode content.
	if err := os.Link(target, filepath.Join(dir, "index.js")); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenBytes("test.tar", createTestTar())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	if err := ExtractAll(reader, dir); err != nil {
		t.Fatal(err)
	}

	info, err := os.Lstat(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		t.Fatal("destination symlink was followed, not replaced")
	}
	assertFileContent(t, filepath.Join(dir, "package.json"), `{"name": "test"}`)
	assertFileContent(t, filepath.Join(dir, "index.js"), "console.log('hello');")
	// Original inode behind the hard link was not modified.
	assertFileContent(t, target, "original")
}

func TestExtractAllSkipsTarSpecialEntries(t *testing.T) {
	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)
	if err := tw.WriteHeader(&tar.Header{
		Name:     "link",
		Typeflag: tar.TypeSymlink,
		Linkname: "../outside",
		Mode:     0o777,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name:     "dev",
		Typeflag: tar.TypeChar,
		Mode:     0o644,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name:     "hardlink",
		Typeflag: tar.TypeLink,
		Linkname: "regular.txt",
		Mode:     0o644,
	}); err != nil {
		t.Fatal(err)
	}
	writeTarFile(t, tw, "regular.txt", "ok", 0o644)
	_ = tw.Close()

	reader, err := OpenBytes("test.tar", buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	dir := t.TempDir()
	if err := ExtractAll(reader, dir); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"link", "dev", "hardlink"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s entry was written: %v", name, err)
		}
	}
	assertFileContent(t, filepath.Join(dir, "regular.txt"), "ok")
}

func TestExtractAllSkipsZipSymlink(t *testing.T) {
	buf := new(bytes.Buffer)
	zw := zip.NewWriter(buf)
	hdr := &zip.FileHeader{Name: "link", Method: zip.Store}
	hdr.SetMode(fs.ModeSymlink | 0o777)
	w, _ := zw.CreateHeader(hdr)
	_, _ = w.Write([]byte("../target"))
	f, _ := zw.Create("regular.txt")
	_, _ = f.Write([]byte("ok"))
	_ = zw.Close()

	reader, err := OpenBytes("test.zip", buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	dir := t.TempDir()
	if err := ExtractAll(reader, dir); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(filepath.Join(dir, "link")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("symlink entry was written: %v", err)
	}
	assertFileContent(t, filepath.Join(dir, "regular.txt"), "ok")
}

func TestExtractAllWithPrefix(t *testing.T) {
	reader, err := OpenBytesWithPrefix("test.zip", createTestZip(), "src/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	dir := t.TempDir()
	if err := ExtractAll(reader, dir); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, filepath.Join(dir, "main.go"), "package main")
	assertFileContent(t, filepath.Join(dir, "util", "helper.go"), "package util")
}

func createTarWithEntry(t *testing.T, name string) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)
	writeTarFile(t, tw, name, "x", 0o644)
	_ = tw.Close()
	return buf.Bytes()
}

func writeTarFile(t *testing.T, tw *tar.Writer, name, content string, mode int64) {
	t.Helper()
	err := tw.WriteHeader(&tar.Header{
		Name: name,
		Size: int64(len(content)),
		Mode: mode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func assertFileMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

func assertNoEscape(t *testing.T, dir string) {
	t.Helper()
	parent := filepath.Dir(dir)
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(dir) {
			t.Fatalf("file %q escaped into %s", e.Name(), parent)
		}
	}
}
