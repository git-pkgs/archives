package archives

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"math"
	"testing"
)

func skipZIPFixture(t *testing.T, method uint16) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, entry := range []struct{ name, body string }{{"big", string(bytes.Repeat([]byte{'x'}, 2700))}, {"small", "valid"}, {"tail", "end"}} {
		body, err := w.CreateHeader(&zip.FileHeader{Name: entry.name, Method: method})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(body, entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestStreamSkipRejectedZIPEntry(t *testing.T) {
	for _, method := range []uint16{zip.Store, zip.Deflate} {
		for _, options := range []StreamOptions{{MaxEntryBytes: 5}, {MaxExpandedBytes: 5}} {
			assertSkipRejectedZIPEntry(t, method, options)
		}
	}
}

func assertSkipRejectedZIPEntry(t *testing.T, method uint16, options StreamOptions) {
	t.Helper()
	s, err := OpenStreamBytes("renamed", skipZIPFixture(t, method), options)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := s.Next()
	if entry == nil || entry.Path != "big" || (!errors.Is(err, ErrEntrySizeLimit) && !errors.Is(err, ErrDecompressLimit)) {
		t.Fatalf("rejected entry: %+v %v", entry, err)
	}
	if _, again := s.Next(); again != err {
		t.Fatal("size rejection should remain terminal until Skip")
	}
	if err := s.Skip(); err != nil {
		t.Fatal(err)
	}
	entry, err = s.Next()
	if err != nil || entry.Path != "small" {
		t.Fatalf("small entry: %+v %v", entry, err)
	}
	body, err := io.ReadAll(s)
	if err != nil || string(body) != "valid" {
		t.Fatalf("small body: %q %v", body, err)
	}
	_, err = s.Next()
	if options.MaxExpandedBytes > 0 && !errors.Is(err, ErrDecompressLimit) {
		t.Fatalf("read bytes lost from budget: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStreamSkipUnreadZIPEntry(t *testing.T) {
	s, err := OpenStreamBytes("renamed", skipZIPFixture(t, zip.Deflate), StreamOptions{MaxExpandedBytes: 2700})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Skip(); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatal(err)
	}
	entry, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	entry.Size = math.MaxInt64
	if err := s.Skip(); err != nil {
		t.Fatal(err)
	}
	if err := s.Skip(); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatal(err)
	}
	entry, err = s.Next()
	if err != nil || entry.Path != "small" {
		t.Fatalf("reserved bytes not released: %+v %v", entry, err)
	}
	if _, err := s.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.Skip(); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("partially read entry skipped: %v", err)
	}
	if err := consumeStream(s, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Skip(); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatal(err)
	}
}

func TestStreamSkipDoesNotReadRejectedBody(t *testing.T) {
	for _, method := range []uint16{zip.Store, zip.Deflate} {
		s, err := OpenStreamBytes("renamed", corruptZipMember(t, method), StreamOptions{MaxExpandedBytes: 5})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Next(); !errors.Is(err, ErrDecompressLimit) {
			t.Fatal(err)
		}
		if err := s.Skip(); err != nil {
			t.Fatal(err)
		}
		entry, err := s.Next()
		if err != nil || entry.Path != "valid" {
			t.Fatalf("next after corrupt skipped body: %+v %v", entry, err)
		}
		body, err := io.ReadAll(s)
		if err != nil || string(body) != "valid" {
			t.Fatalf("body: %q %v", body, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStreamSkipUnsupportedFormatsAndCancellation(t *testing.T) {
	for _, name := range []string{"test.tar", "test.tar.gz", "test.conda"} {
		s, err := OpenStreamBytes(name, streamTestArchive(t, name, tarWithPayloads(t, make([]byte, 2700))), StreamOptions{MaxEntryBytes: 10})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Next(); !errors.Is(err, ErrEntrySizeLimit) {
			t.Fatal(err)
		}
		if err := s.Skip(); !errors.Is(err, errors.ErrUnsupported) {
			t.Fatalf("%s: %v", name, err)
		}
		_ = s.Close()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s, err := OpenStreamBytesContext(ctx, "renamed", skipZIPFixture(t, zip.Store), StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next(); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := s.Skip(); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_ = s.Close()
	if err := s.Skip(); !errors.Is(err, fs.ErrClosed) {
		t.Fatal(err)
	}
	if _, err := OpenStreamBytes("renamed", skipZIPFixture(t, zip.Store), StreamOptions{MaxEntries: 2}); !errors.Is(err, ErrEntryLimit) {
		t.Fatalf("entry count limit bypassed: %v", err)
	}
}
