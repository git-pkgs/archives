package archives

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

var hexOrder = []string{"VERSION", "CHECKSUM", "metadata.config", hexContents}

func hexMembers(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	contents := string(gzipTar(t, raw))
	metadata := "{<<\"name\">>,<<\"example\">>}.\n"
	checksum := sha256.Sum256([]byte("3" + metadata + contents))
	return map[string]string{"VERSION": "3", "CHECKSUM": fmt.Sprintf("%X", checksum), "metadata.config": metadata, hexContents: contents}
}

func hexTar(t *testing.T, order []string, members map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range order {
		writeTarFile(t, tw, name, members[name], 0o644)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func hexOrders(names []string) [][]string {
	if len(names) == 0 {
		return [][]string{nil}
	}
	var orders [][]string
	for i, name := range names {
		rest := append(append([]string{}, names[:i]...), names[i+1:]...)
		for _, tail := range hexOrders(rest) {
			orders = append(orders, append([]string{name}, tail...))
		}
	}
	return orders
}

func TestHexPublicReaders(t *testing.T) {
	raw := browseTar(t, "root/", "root/README.md", "root/src/main.go", "root/README.md")
	want, err := OpenBytes("inner.tar", raw)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = want.Close() }()
	files, err := want.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range hexOrders(hexOrder) {
		t.Run(strings.Join(order, ","), func(t *testing.T) {
			data := hexTar(t, order, hexMembers(t, raw))
			for _, name := range []string{"example-1.0.0.tar", "download"} {
				for _, open := range []func() (Reader, error){
					func() (Reader, error) { return Open(name, bytes.NewReader(data)) },
					func() (Reader, error) { return OpenBytes(name, data) },
				} {
					r, err := open()
					if err != nil {
						t.Fatal(err)
					}
					assertHexReader(t, r, files, data)
					_ = r.Close()
				}
				for _, open := range []func() (*Stream, error){
					func() (*Stream, error) { return OpenStream(name, bytes.NewReader(data), StreamOptions{}) },
					func() (*Stream, error) { return OpenStreamBytes(name, data, StreamOptions{}) },
				} {
					s, err := open()
					if err != nil {
						t.Fatal(err)
					}
					assertStreamMatches(t, s, want, files)
					_ = s.Close()
				}
				assertHexBrowser(t, name, data, files)
			}
		})
	}
}

func assertHexReader(t *testing.T, r Reader, files []FileInfo, data []byte) {
	t.Helper()
	got, err := r.List()
	if err != nil || !reflect.DeepEqual(got, files) {
		t.Fatalf("List: %+v, %v", got, err)
	}
	assertTarPayload(t, r, "root/README.md", []byte("body: root/README.md"))
	hash, err := r.Hash("sha256")
	if err != nil || hash != fmt.Sprintf("%x", sha256.Sum256(data)) {
		t.Fatalf("outer Hash: %s, %v", hash, err)
	}
}

func assertHexBrowser(t *testing.T, name string, data []byte, files []FileInfo) {
	t.Helper()
	b, err := OpenBrowser(name, bytes.NewReader(data), func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}, BrowseOptions{DetectRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.List()
	if err != nil || !reflect.DeepEqual(got, stripFilePrefix(files, "root/")) {
		t.Fatalf("Browser.List: %+v, %v", got, err)
	}
	assertBrowsedBody(t, b, "README.md", "body: root/README.md")
}

func TestHexDetectionLeavesOrdinaryTar(t *testing.T) {
	for _, tc := range []struct {
		name   string
		order  []string
		change string
		value  string
	}{
		{"only-contents", []string{hexContents}, "", ""},
		{"missing-checksum", []string{"VERSION", "metadata.config", hexContents}, "", ""},
		{"duplicate", []string{"VERSION", "VERSION", "CHECKSUM", "metadata.config", hexContents}, "", ""},
		{"version", hexOrder, "VERSION", "2"},
		{"checksum", hexOrder, "CHECKSUM", strings.Repeat("Z", 64)},
		{"short-checksum", hexOrder, "CHECKSUM", "1234"},
		{"unrelated", []string{"README.md", hexContents}, "README.md", "hello"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			members := hexMembers(t, browseTar(t, "inner"))
			members[tc.change] = tc.value
			data := hexTar(t, tc.order, members)
			r, err := OpenBytes("example.tar", data)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			files, err := r.List()
			if err != nil || len(files) != len(tc.order) {
				t.Fatalf("List: %+v, %v", files, err)
			}
			for i, f := range files {
				if f.Path != tc.order[i] {
					t.Fatalf("path = %s, want %s", f.Path, tc.order[i])
				}
			}
			s, err := OpenStream("example.tar", bytes.NewReader(data), StreamOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			assertStreamMatches(t, s, r, files)
		})
	}
}

func TestHexLimitsAndInvalidPayload(t *testing.T) {
	members := hexMembers(t, browseTar(t, "README.md", "other"))
	for _, order := range [][]string{hexOrder, {hexContents, "VERSION", "CHECKSUM", "metadata.config"}} {
		data := hexTar(t, order, members)
		for _, tc := range []struct {
			options StreamOptions
			want    error
		}{
			{StreamOptions{MaxEntryBytes: 2}, ErrEntrySizeLimit},
			{StreamOptions{MaxExpandedBytes: 20}, ErrDecompressLimit},
			{StreamOptions{MaxEntries: 3}, ErrEntryLimit},
			{StreamOptions{MaxInputBytes: 600}, ErrInputLimit},
		} {
			b, err := OpenBrowser("example.tar", bytes.NewReader(data), nil, BrowseOptions{Limits: tc.options})
			if err != nil {
				t.Fatal(err)
			}
			files, err := b.List()
			if !errors.Is(err, tc.want) || files != nil {
				t.Fatalf("List = %+v, %v, want %v", files, err, tc.want)
			}
		}
	}
	members[hexContents] = "invalid gzip"
	data := hexTar(t, hexOrder, members)
	if r, err := OpenBytes("example.tar", data); err == nil {
		_ = r.Close()
		t.Fatal("OpenBytes accepted invalid payload")
	}
	s, err := OpenStreamBytes("example.tar", data, StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Next(); err == nil {
		t.Fatal("Next accepted invalid payload")
	}
}

func TestHexCancellation(t *testing.T) {
	for _, order := range [][]string{hexOrder, {hexContents, "VERSION", "CHECKSUM", "metadata.config"}} {
		ctx := newCheckContext(t)
		data := hexTar(t, order, hexMembers(t, cancellationTar(t)))
		b, err := OpenBrowserContext(ctx, "example.tar", bytes.NewReader(data), nil, BrowseOptions{})
		if err != nil {
			t.Fatal(err)
		}
		ctx.remaining = 10
		if _, err := b.List(); !errors.Is(err, context.Canceled) {
			t.Fatalf("List during detection: %v", err)
		}
		ctx = newCheckContext(t)
		b, err = OpenBrowserContext(ctx, "example.tar", bytes.NewReader(data), nil, BrowseOptions{})
		if err != nil {
			t.Fatal(err)
		}
		file, err := b.Extract("root/padding")
		if err != nil {
			t.Fatal(err)
		}
		ctx.remaining = 10
		_, err = io.Copy(io.Discard, file)
		_ = file.Close()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Read during decoding: %v", err)
		}
	}
}

func TestHexStreamsContentsLast(t *testing.T) {
	payload := make([]byte, 1<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	data := hexTar(t, hexOrder, hexMembers(t, tarWithPayloads(t, payload)))
	input := &countStreamInput{Reader: bytes.NewReader(data)}
	s, err := OpenStream("example.tar", input, StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Next(); err != nil {
		t.Fatal(err)
	}
	if input.bytes >= len(data)/2 {
		t.Fatalf("read %d of %d bytes before returning first inner header", input.bytes, len(data))
	}
	got, err := io.ReadAll(s)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("inner payload mismatch: %v", err)
	}
	if input.closed {
		t.Fatal("closed caller input")
	}
}

func TestTarProbePreservesInputError(t *testing.T) {
	data := hexTar(t, hexOrder, hexMembers(t, browseTar(t, "README.md")))
	want := errors.New("input failed")
	input := io.MultiReader(bytes.NewReader(data[:600]), iotest.ErrReader(want))
	s, err := OpenStream("example.tar", input, StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := consumeStream(s, false); !errors.Is(err, want) {
		t.Fatalf("lost input error: %v", err)
	}
}
