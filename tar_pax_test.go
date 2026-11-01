package archives

import (
	"archive/tar"
	"bytes"
	"io"
	"reflect"
	"testing"
)

func tarWithGlobalPAXHeaders(t *testing.T, prefix string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range []string{"README.md", "pax_global_header", ""} {
		if err := tw.WriteHeader(&tar.Header{
			Name:       "pax_global_header",
			Typeflag:   tar.TypeXGlobalHeader,
			PAXRecords: map[string]string{"comment": "0123456789abcdef0123456789abcdef01234567"},
		}); err != nil {
			t.Fatal(err)
		}
		if name != "" {
			writeTarFile(t, tw, prefix+name, "body: "+name, 0o644)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestTarSkipsGlobalPAXHeaders(t *testing.T) {
	raw := tarWithGlobalPAXHeaders(t, "")
	for _, name := range []string{"test.tar", "test.tar.gz", "test.tar.xz", "test.tar.zst", "test.gem", "test.conda"} {
		t.Run(name, func(t *testing.T) {
			data := streamTestArchive(t, name, raw)
			r, err := OpenBytes(name, data)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			files, err := r.List()
			if err != nil {
				t.Fatal(err)
			}
			var paths []string
			for _, file := range files {
				paths = append(paths, file.Path)
				body, err := r.Extract(file.Path)
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(body)
				_ = body.Close()
				if err != nil || string(got) != "body: "+file.Path {
					t.Fatalf("Extract(%q) = %q, %v", file.Path, got, err)
				}
			}
			if !reflect.DeepEqual(paths, []string{"README.md", "pax_global_header"}) {
				t.Fatalf("paths = %v", paths)
			}
		})
	}
}

func TestStreamSkipsGlobalPAXHeaders(t *testing.T) {
	raw := tarWithGlobalPAXHeaders(t, "")
	for _, name := range []string{"test.tar", "test.tar.gz", "test.tar.xz", "test.tar.zst", "test.gem", "test.conda"} {
		t.Run(name, func(t *testing.T) {
			data := streamTestArchive(t, name, raw)
			s, err := OpenStream(name, bytes.NewReader(data), StreamOptions{MaxEntries: 2})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			for _, path := range []string{"README.md", "pax_global_header"} {
				entry, err := s.Next()
				if err != nil {
					t.Fatal(err)
				}
				if entry.Path != path {
					t.Fatalf("path = %q, want %q", entry.Path, path)
				}
				body, err := io.ReadAll(s)
				if err != nil || string(body) != "body: "+path {
					t.Fatalf("Read(%q) = %q, %v", path, body, err)
				}
			}
			if _, err := s.Next(); err != io.EOF {
				t.Fatalf("end = %v, want EOF", err)
			}
		})
	}
}

func TestBrowserRootWithGlobalPAXHeaders(t *testing.T) {
	data := gzipTar(t, tarWithGlobalPAXHeaders(t, "repo-main/"))
	r, err := OpenBytes("test.tar.gz", data)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	files, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if root := CommonRoot(files); root != "repo-main/" {
		t.Fatalf("CommonRoot = %q", root)
	}
	reopen := func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	b, err := OpenBrowser("test.tar.gz", bytes.NewReader(data), reopen, BrowseOptions{DetectRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	files, err = b.ListDir("")
	if err != nil || len(files) != 2 || files[0].Path != "README.md" || files[1].Path != "pax_global_header" {
		t.Fatalf("ListDir = %+v, %v", files, err)
	}
	assertBrowsedBody(t, b, "README.md", "body: README.md")
	assertBrowsedBody(t, b, "pax_global_header", "body: pax_global_header")
}
