package archives

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"reflect"
	"strings"
	"testing"
)

func browseTar(t *testing.T, names ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range names {
		if strings.HasSuffix(name, "/") {
			if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir}); err != nil {
				t.Fatal(err)
			}
		} else {
			writeTarFile(t, tw, name, "body: "+name, 0o644)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestBrowserFormats(t *testing.T) {
	raw := browseTar(t, "root/", "root/README.md", "root/src/main.go", "root/src/util.go")
	for _, name := range []string{"test.tar", "test.tar.gz", "test.tar.xz", "test.tar.zst", "test.zip", "test.gem", "test.conda"} {
		t.Run(name, func(t *testing.T) {
			data := streamTestArchive(t, name, raw)
			first := &countStreamInput{Reader: bytes.NewReader(data)}
			var reopened []*countStreamInput
			reopen := func() (io.ReadCloser, error) {
				r := &countStreamInput{Reader: bytes.NewReader(data)}
				reopened = append(reopened, r)
				return r, nil
			}
			b, err := OpenBrowser(name, first, reopen, BrowseOptions{DetectRoot: true})
			if err != nil {
				t.Fatal(err)
			}
			buffered, err := OpenBytesWithPrefix(name, data, "root/")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = buffered.Close() }()
			assertBrowserListings(t, b, buffered)
			assertBrowsedBody(t, b, "README.md", "body: root/README.md")
			if first.closed {
				t.Error("browser closed caller's input")
			}
			if name == "test.zip" || name == "test.conda" {
				if len(reopened) != 0 {
					t.Error("random-access input was reopened")
				}
			} else if len(reopened) != 4 {
				t.Errorf("reopened %d times, want 4", len(reopened))
			}
			for _, r := range reopened {
				if !r.closed {
					t.Error("reopened input left open")
				}
			}
		})
	}
}

func assertBrowserListings(t *testing.T, b *Browser, buffered Reader) {
	t.Helper()
	for _, dir := range []string{"", "src", "missing"} {
		got, err := b.ListDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		want, err := buffered.ListDir(dir)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("ListDir(%q) = %+v, want %+v; %v", dir, got, want, err)
		}
	}
}

func assertBrowsedBody(t *testing.T, b *Browser, name, want string) {
	t.Helper()
	r, err := b.Extract(name)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if closeErr := r.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || string(got) != want {
		t.Fatalf("Extract(%q) = %q, %v; want %q", name, got, err, want)
	}
}

func TestBrowserRootDetection(t *testing.T) {
	for _, tc := range []struct {
		names, paths []string
		prefix       string
		detect       bool
	}{
		{[]string{"README.md"}, []string{"README.md"}, "", true},
		{[]string{"root/README.md"}, []string{"README.md"}, "root/", true},
		{[]string{"root/", "root/README.md"}, []string{"README.md"}, "root/", true},
		{[]string{"README.md", "src/main.go"}, []string{"README.md", "src/"}, "", true},
		{[]string{"root/README.md", "other/main.go"}, []string{"root/", "other/"}, "", true},
		{[]string{"root/README.md"}, []string{"root/"}, "", false},
		{nil, nil, "", true},
	} {
		t.Run(strings.Join(tc.names, ","), func(t *testing.T) {
			data := browseTar(t, tc.names...)
			reopen := func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
			b, err := OpenBrowser("test.tar", bytes.NewReader(data), reopen, BrowseOptions{DetectRoot: tc.detect})
			if err != nil {
				t.Fatal(err)
			}
			files, err := b.ListDir("")
			if err != nil {
				t.Fatal(err)
			}
			var paths []string
			for _, file := range files {
				paths = append(paths, file.Path)
			}
			if !reflect.DeepEqual(paths, tc.paths) {
				t.Fatalf("paths = %v, want %v", paths, tc.paths)
			}
			for _, name := range tc.names {
				if !strings.HasSuffix(name, "/") {
					assertBrowsedBody(t, b, strings.TrimPrefix(name, tc.prefix), "body: "+name)
				}
			}
		})
	}
}

func TestBrowserBzip2(t *testing.T) {
	data := createTestTarBz2(t)
	reopen := func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	b, err := OpenBrowser("test.tar.bz2", bytes.NewReader(data), reopen, BrowseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := OpenBytes("test.tar.bz2", data)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	assertBrowserListings(t, b, r)
	files, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.IsDir {
			continue
		}
		file, err := r.Extract(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(file)
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
		assertBrowsedBody(t, b, f.Path, string(body))
	}
}

func TestBrowserPrefixAndDuplicates(t *testing.T) {
	data := browseTar(t, "package/a", "package/a", "other/b")
	b, err := OpenBrowser("test.tar", bytes.NewReader(data), func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}, BrowseOptions{StripPrefix: "package/", DetectRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	files, err := b.ListDir("")
	if err != nil || len(files) != 2 || files[0].Path != "a" || files[1].Path != "a" {
		t.Fatalf("listing = %+v, %v", files, err)
	}
	assertBrowsedBody(t, b, "a", "body: package/a")
	if _, err := b.Extract("other/b"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestBrowserContentDetection(t *testing.T) {
	for _, format := range []string{"test.zip", "test.tar.gz"} {
		data := streamTestArchive(t, format, browseTar(t, "README.md"))
		for _, name := range []string{"artifact", "package.apk"} {
			b, err := OpenBrowser(name, bytes.NewReader(data), nil, BrowseOptions{})
			if err != nil {
				t.Fatal(err)
			}
			assertBrowsedBody(t, b, "README.md", "body: README.md")
		}
	}
}

func TestBrowserLimits(t *testing.T) {
	for _, name := range []string{"test.tar", "test.zip", "test.conda"} {
		data := streamTestArchive(t, name, browseTar(t, "a", "b"))
		for _, tc := range []struct {
			limits StreamOptions
			want   error
		}{
			{StreamOptions{MaxInputBytes: 1}, ErrInputLimit},
			{StreamOptions{MaxEntryBytes: 1}, ErrEntrySizeLimit},
			{StreamOptions{MaxExpandedBytes: 1}, ErrDecompressLimit},
			{StreamOptions{MaxEntries: 1}, ErrEntryLimit},
		} {
			b, err := OpenBrowser(name, bytes.NewReader(data), nil, BrowseOptions{Limits: tc.limits})
			if err == nil {
				_, err = b.ListDir("")
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("%s: error = %v, want %v", name, err, tc.want)
			}
		}
	}
	if _, err := OpenBrowser("test.tar", bytes.NewReader(nil), nil, BrowseOptions{Limits: StreamOptions{MaxEntries: -1}}); err == nil {
		t.Fatal("negative limit accepted")
	}
}

func TestBrowserReopenErrors(t *testing.T) {
	data := browseTar(t, "dir/", "dir/a")
	wantErr := errors.New("storage unavailable")
	for _, tc := range []struct {
		reopen func() (io.ReadCloser, error)
		want   error
	}{
		{nil, errors.ErrUnsupported},
		{func() (io.ReadCloser, error) { return nil, wantErr }, wantErr},
	} {
		b, err := OpenBrowser("test.tar", bytes.NewReader(data), tc.reopen, BrowseOptions{DetectRoot: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.Extract("a"); !errors.Is(err, tc.want) {
			t.Fatalf("Extract error = %v, want %v", err, tc.want)
		}
	}
	closed := &countStreamInput{Reader: strings.NewReader("not gzip")}
	b, err := OpenBrowser("test.tar.gz", bytes.NewReader(gzipTar(t, data)), func() (io.ReadCloser, error) { return closed, nil }, BrowseOptions{DetectRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Extract("a"); err == nil || !closed.closed {
		t.Fatalf("error=%v, closed=%t", err, closed.closed)
	}
}

func TestBrowserExtractClosesReopenedInput(t *testing.T) {
	data := browseTar(t, "dir/", "dir/a")
	for _, target := range []string{"dir/a", "dir/", "missing"} {
		input := &countStreamInput{Reader: bytes.NewReader(data)}
		b, err := OpenBrowser("test.tar", bytes.NewReader(data), func() (io.ReadCloser, error) { return input, nil }, BrowseOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.ListDir(""); err != nil {
			t.Fatal(err)
		}
		file, err := b.Extract(target)
		switch target {
		case "dir/":
			if !errors.Is(err, ErrIsDir) {
				t.Fatal(err)
			}
		case "missing":
			if !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
		default:
			if err != nil || input.closed {
				t.Fatalf("error=%v closed=%t", err, input.closed)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := file.Read(make([]byte, 1)); !errors.Is(err, fs.ErrClosed) {
				t.Fatal(err)
			}
		}
		if !input.closed {
			t.Error("reopened input left open")
		}
	}
}

func TestBrowserExtractDirectories(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []string
		options BrowseOptions
		target  string
	}{
		{"explicit without slash", []string{"dir/", "dir/a"}, BrowseOptions{}, "dir"},
		{"explicit with slash", []string{"dir/", "dir/a"}, BrowseOptions{}, "dir/"},
		{"implicit without slash", []string{"dir/a"}, BrowseOptions{}, "dir"},
		{"implicit with slash", []string{"dir/a"}, BrowseOptions{}, "dir/"},
		{"nested implicit", []string{"dir/sub/a"}, BrowseOptions{}, "dir/sub"},
		{"stripped root", []string{"pkg/dir/a"}, BrowseOptions{DetectRoot: true}, "dir"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := browseTar(t, tc.entries...)
			b, err := OpenBrowser("test.tar", bytes.NewReader(data), func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(data)), nil
			}, tc.options)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := b.Extract(tc.target); !errors.Is(err, ErrIsDir) {
				t.Fatalf("Extract(%q) error = %v, want ErrIsDir", tc.target, err)
			}
		})
	}

	data := browseTar(t, "dir/a", "dirx")
	b, err := OpenBrowser("test.tar", bytes.NewReader(data), nil, BrowseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Extract("di"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Extract(di) error = %v, want fs.ErrNotExist", err)
	}
}
