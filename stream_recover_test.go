package archives

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func corruptZipMember(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, name := range []string{"broken", "valid"} {
		member, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
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
	data[offset] ^= 1
	return data
}

func TestStreamRecoverZIP(t *testing.T) {
	for _, skip := range []bool{false, true} {
		s, err := OpenStreamBytes("test.zip", corruptZipMember(t), StreamOptions{})
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
		if !errors.Is(err, zip.ErrChecksum) {
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
}

func TestStreamRecoverPreservesLimitsAndCancellation(t *testing.T) {
	for _, options := range []StreamOptions{{MaxEntryBytes: 1}, {MaxExpandedBytes: 1}} {
		s, err := OpenStreamBytes("test.zip", corruptZipMember(t), options)
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
	s, err := OpenStreamBytesContext(ctx, "test.zip", corruptZipMember(t), StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := s.Recover(); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_ = s.Close()
}
