package archives

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"testing"
	"testing/fstest"
	"time"
)

func openTestFS(t *testing.T, name string, data []byte, prefix string) *FS {
	t.Helper()
	r, err := OpenBytesWithPrefix(name, data, prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	f, err := NewFS(r)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestFSFormats(t *testing.T) {
	for _, tc := range []struct {
		name   string
		data   []byte
		prefix string
		files  []string
	}{
		{"test.zip", createTestZip(), "", []string{"README.md", "src/main.go", "src/util/helper.go", "docs/guide.md"}},
		{"test.tar.gz", createTestTarGz(), "", []string{"package.json", "index.js", "lib/util.js"}},
		{"test.gem", createTestGem(), "", []string{"lib/main.rb"}},
		{"test.conda", createTestConda(t), "", []string{"site-packages/six.py", "info/index.json", "info/paths.json", "info/licenses/LICENSE", "site-packages/six-1.0/META"}},
		{"prefix.zip", createTestZipWithDirEntries(), "project-abc123/", []string{"README.md", "src/main.go", "src/util.go"}},
		{"prefix.tgz", createTestTarGzWithDirEntries(), "project-abc123/", []string{"README.md", "src/main.go", "src/util.go"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := openTestFS(t, tc.name, tc.data, tc.prefix)
			if err := fstest.TestFS(f, tc.files...); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFSWalkAndSub(t *testing.T) {
	f := openTestFS(t, "test.zip", createTestZip(), "")
	var paths []string
	err := fs.WalkDir(f, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, name)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".", "README.md", "docs", "docs/guide.md", "src", "src/main.go", "src/util", "src/util/helper.go"}
	if !slices.Equal(paths, want) {
		t.Fatalf("walk = %v, want %v", paths, want)
	}
	sub, err := fs.Sub(f, "src")
	if err != nil {
		t.Fatal(err)
	}
	data, err := fs.ReadFile(sub, "util/helper.go")
	if err != nil || string(data) != "package util" {
		t.Fatalf("ReadFile = %q, %v", data, err)
	}
}

func TestFSPathErrors(t *testing.T) {
	f := openTestFS(t, "test.zip", createTestZip(), "")
	operations := map[string]func(string) error{
		"open": func(name string) error {
			file, err := f.Open(name)
			if file != nil {
				_ = file.Close()
			}
			return err
		},
		"stat":     func(name string) error { _, err := fs.Stat(f, name); return err },
		"readdir":  func(name string) error { _, err := fs.ReadDir(f, name); return err },
		"readfile": func(name string) error { _, err := fs.ReadFile(f, name); return err },
	}
	for op, run := range operations {
		t.Run(op, func(t *testing.T) {
			for _, name := range []string{"", "/", "/README.md", "./README.md", "src/", "src//main.go", "src/../README.md", "../README.md", "\xff"} {
				assertFSPathError(t, run(name), name, fs.ErrInvalid)
			}
			for _, name := range []string{"missing", "missing/file", "README.md/child"} {
				assertFSPathError(t, run(name), name, fs.ErrNotExist)
			}
		})
	}
	_, err := fs.ReadFile(f, "src")
	assertFSPathError(t, err, "src", fs.ErrInvalid)
	_, err = fs.ReadDir(f, "README.md")
	assertFSPathError(t, err, "README.md", fs.ErrInvalid)
}

func assertFSPathError(t *testing.T, err error, name string, want error) {
	t.Helper()
	var pe *fs.PathError
	if !errors.Is(err, want) || !errors.As(err, &pe) || pe.Path != name {
		t.Fatalf("error = %v, want PathError for %q wrapping %v", err, name, want)
	}
}

func TestFSDirectoryReads(t *testing.T) {
	f := openTestFS(t, "test.zip", createTestZip(), "")
	dir, err := f.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	rd := dir.(fs.ReadDirFile)
	for _, want := range []string{"README.md", "docs", "src"} {
		entries, err := rd.ReadDir(1)
		if err != nil || len(entries) != 1 || entries[0].Name() != want {
			t.Fatalf("ReadDir(1) = %v, %v, want %s", entries, err, want)
		}
	}
	entries, err := rd.ReadDir(1)
	if len(entries) != 0 || err != io.EOF {
		t.Fatalf("ReadDir at EOF = %v, %v", entries, err)
	}
	entries, err = rd.ReadDir(0)
	if entries == nil || len(entries) != 0 || err != nil {
		t.Fatalf("ReadDir(0) at EOF = %v, %v", entries, err)
	}
	entries, err = f.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	entries[0] = nil
	again, err := f.ReadDir(".")
	if err != nil || again[0] == nil {
		t.Fatalf("caller changed stored directory entries: %v, %v", again, err)
	}
}

func TestFSClosedFiles(t *testing.T) {
	f := openTestFS(t, "test.zip", createTestZip(), "")
	for _, name := range []string{"README.md", "."} {
		file, err := f.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		_, err = file.Read(make([]byte, 1))
		assertFSPathError(t, err, name, fs.ErrClosed)
		_, err = file.Stat()
		assertFSPathError(t, err, name, fs.ErrClosed)
		_, err = file.(fs.ReadDirFile).ReadDir(1)
		assertFSPathError(t, err, name, fs.ErrClosed)
		assertFSPathError(t, file.Close(), name, fs.ErrClosed)
	}
	data, err := fs.ReadFile(f, "README.md")
	if err != nil || string(data) != "# Test Package" {
		t.Fatalf("read after closing other files = %q, %v", data, err)
	}
}

func TestFSArchivePathsAndMetadata(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	modified := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	writeTarFile(t, tw, "./lib/tool", "first", 0o750)
	writeTarFile(t, tw, "./lib/tool", "duplicate", 0o600)
	for _, header := range []*tar.Header{
		{Name: "./", Typeflag: tar.TypeDir, Mode: 0o700, ModTime: modified},
		{Name: "./lib/", Typeflag: tar.TypeDir, Mode: 0o710, ModTime: modified},
		{Name: "empty/", Typeflag: tar.TypeDir, Mode: 0o755},
	} {
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
	}
	writeTarFile(t, tw, " space /file", "space", 0o000)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	f := openTestFS(t, "test.tar", buf.Bytes(), "")
	if err := fstest.TestFS(f, "lib/tool", "empty", " space /file"); err != nil {
		t.Fatal(err)
	}
	data, err := fs.ReadFile(f, "lib/tool")
	if err != nil || string(data) != "first" {
		t.Fatalf("ReadFile = %q, %v", data, err)
	}
	for _, tc := range []struct {
		name string
		mode fs.FileMode
		size int64
	}{
		{".", fs.ModeDir | 0o700, 0},
		{"lib", fs.ModeDir | 0o710, 0},
		{"lib/tool", 0o750, 5},
		{" space /file", 0, 5},
	} {
		info, err := fs.Stat(f, tc.name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode() != tc.mode || info.Size() != tc.size {
			t.Errorf("%s: mode/size = %v/%d, want %v/%d", tc.name, info.Mode(), info.Size(), tc.mode, tc.size)
		}
		if info.IsDir() && !info.ModTime().Equal(modified) {
			t.Errorf("%s: ModTime = %v, want %v", tc.name, info.ModTime(), modified)
		}
	}
}

func TestFSBackslashName(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	writeTarFile(t, tw, "a\\b", "backslash", 0o644)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	f := openTestFS(t, "test.tar", buf.Bytes(), "")
	data, err := fs.ReadFile(f, "a\\b")
	if err != nil || string(data) != "backslash" {
		t.Fatalf("ReadFile = %q, %v", data, err)
	}
	entries, err := fs.ReadDir(f, ".")
	if err != nil || len(entries) != 1 || entries[0].Name() != "a\\b" {
		t.Fatalf("ReadDir = %v, %v", entries, err)
	}
}

func TestFSRejectsInvalidArchivePaths(t *testing.T) {
	for _, names := range [][]string{
		{"../escape"}, {"/absolute"}, {"a/../b"}, {"a//b"}, {"a/./b"}, {"."}, {".//"}, {"/"},
		{"file", "file/child"}, {"file/child", "file"}, {"file", "file/"},
	} {
		t.Run(fmt.Sprint(names), func(t *testing.T) {
			var buf bytes.Buffer
			zw := zip.NewWriter(&buf)
			for _, name := range names {
				if _, err := zw.Create(name); err != nil {
					t.Fatal(err)
				}
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			r, err := OpenBytes("test.zip", buf.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			if _, err := NewFS(r); !errors.Is(err, fs.ErrInvalid) {
				t.Fatalf("NewFS = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestFSEmptyArchive(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f := openTestFS(t, "empty.zip", buf.Bytes(), "")
	if err := fstest.TestFS(f); err != nil {
		t.Fatal(err)
	}
}

func TestFSSpecialEntries(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, entry := range []struct {
		name string
		kind byte
	}{
		{"symlink", tar.TypeSymlink}, {"hardlink", tar.TypeLink}, {"fifo", tar.TypeFifo},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Typeflag: entry.kind, Linkname: "target", Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	f := openTestFS(t, "special.tar", buf.Bytes(), "")
	entries, err := fs.ReadDir(f, ".")
	if err != nil || len(entries) != 3 {
		t.Fatalf("ReadDir = %v, %v", entries, err)
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			t.Errorf("%s reported as regular", entry.Name())
		}
		_, err := f.Open(entry.Name())
		assertFSPathError(t, err, entry.Name(), fs.ErrInvalid)
	}
}

type fsErrorReader struct {
	Reader
	listErr    error
	extractErr error
	content    io.ReadCloser
}

func (r *fsErrorReader) List() ([]FileInfo, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.Reader.List()
}

func (r *fsErrorReader) Extract(name string) (io.ReadCloser, error) {
	return r.content, r.extractErr
}

type fsErrorContent struct {
	err    error
	closed bool
}

func (c *fsErrorContent) Read(b []byte) (int, error) { return 0, c.err }
func (c *fsErrorContent) Close() error {
	c.closed = true
	return nil
}

func TestFSReaderErrors(t *testing.T) {
	r, err := OpenBytes("test.zip", createTestZip())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	want := errors.New("reader failure")
	wrapped := &fsErrorReader{Reader: r, listErr: want}
	_, err = NewFS(wrapped)
	assertFSPathError(t, err, ".", want)
	wrapped.listErr = nil
	wrapped.extractErr = want
	f, err := NewFS(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Stat(f, "README.md"); err != nil {
		t.Fatalf("Stat extracted contents: %v", err)
	}
	_, err = fs.ReadFile(f, "README.md")
	assertFSPathError(t, err, "README.md", want)
	content := &fsErrorContent{err: want}
	wrapped.extractErr = nil
	wrapped.content = content
	_, err = fs.ReadFile(f, "README.md")
	assertFSPathError(t, err, "README.md", want)
	if !content.closed {
		t.Fatal("ReadFile did not close the extracted stream after a read error")
	}
}

func ExampleNewFS() {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("lib/hello.txt")
	if err != nil {
		panic(err)
	}
	if _, err := io.WriteString(w, "hello\n"); err != nil {
		panic(err)
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	r, err := Open("package.zip", &buf)
	if err != nil {
		panic(err)
	}
	defer func() { _ = r.Close() }()
	f, err := NewFS(r)
	if err != nil {
		panic(err)
	}
	err = fs.WalkDir(f, ".", func(name string, entry fs.DirEntry, err error) error {
		if err == nil {
			fmt.Println(name)
		}
		return err
	})
	if err != nil {
		panic(err)
	}
	data, err := fs.ReadFile(f, "lib/hello.txt")
	if err != nil {
		panic(err)
	}
	fmt.Print(string(data))
	// Output:
	// .
	// lib
	// lib/hello.txt
	// hello
}
