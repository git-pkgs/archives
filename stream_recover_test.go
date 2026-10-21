package archives

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"errors"
	"io"
	"testing"
)

func corruptZipMember(t *testing.T, method uint16) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, name := range []string{"broken", "valid"} {
		member, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: method})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(member, name); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	offset, err := reader.File[0].DataOffset()
	if err != nil {
		t.Fatal(err)
	}
	if method == zip.Store {
		data[offset] ^= 1
	} else {
		data[offset] = 0x07 // Reserved DEFLATE block type.
	}
	return data
}

func TestStreamRecoverZIP(t *testing.T) {
	for _, method := range []uint16{zip.Store, zip.Deflate} {
		for _, skip := range []bool{false, true} {
			assertRecoverZIP(t, method, skip)
		}
	}
}

func assertRecoverZIP(t *testing.T, method uint16, skip bool) {
	t.Helper()
	s, err := OpenStreamBytes("test.zip", corruptZipMember(t, method), StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next(); err != nil {
		t.Fatal(err)
	}
	if skip {
		_, err = s.Next()
	} else {
		_, err = io.ReadAll(s)
	}
	var corrupt flate.CorruptInputError
	if (method == zip.Store && !errors.Is(err, zip.ErrChecksum)) || (method == zip.Deflate && !errors.As(err, &corrupt)) {
		t.Fatalf("member error: %v", err)
	}
	if err := s.Recover(); err != nil {
		t.Fatal(err)
	}
	next, err := s.Next()
	if err != nil || next.Path != "valid" {
		t.Fatalf("next: %+v %v", next, err)
	}
	body, err := io.ReadAll(s)
	if err != nil || string(body) != "valid" {
		t.Fatalf("body: %q %v", body, err)
	}
	if _, err := s.Next(); err != io.EOF {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStreamRecoverPreservesLimitsAndCancellation(t *testing.T) {
	for _, options := range []StreamOptions{{MaxEntryBytes: 1}, {MaxExpandedBytes: 1}} {
		s, err := OpenStreamBytes("test.zip", corruptZipMember(t, zip.Store), options)
		if err != nil {
			t.Fatal(err)
		}
		_, original := s.Next()
		if original == nil || s.Recover() != original {
			t.Fatalf("limit bypassed: %v", original)
		}
		_ = s.Close()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s, err := OpenStreamBytesContext(ctx, "test.zip", corruptZipMember(t, zip.Store), StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := s.Recover(); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_ = s.Close()
}

func TestStreamRecoverUnsupported(t *testing.T) {
	raw := tarWithPayloads(t, make([]byte, 1024))[:1023]
	for _, name := range []string{"test.tar", "test.tar.gz", "test.conda"} {
		s, err := OpenStreamBytes(name, streamTestArchive(t, name, raw), StreamOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Next(); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(s); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("%s: %v", name, err)
		}
		if err := s.Recover(); !errors.Is(err, errors.ErrUnsupported) {
			t.Fatalf("%s: %v", name, err)
		}
		_ = s.Close()
	}
}

func TestStreamRecoverWithoutError(t *testing.T) {
	s, err := OpenStreamBytes("test.zip", createTestZip(), StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Recover(); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatal(err)
	}
	if err := consumeStream(s, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatal(err)
	}
}
