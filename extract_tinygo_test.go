//go:build tinygo

package archives_test

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/git-pkgs/archives"
)

func TestExtractAllUnsupported(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("package/README.md")
	if err != nil {
		t.Fatal(err)
	}
	const content = "# Test package\n"
	if _, err := io.WriteString(w, content); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := archives.OpenBytes("package.zip", buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	for _, opts := range [][]archives.ExtractOption{nil, {archives.WithMaxBytes(1024)}} {
		err := archives.ExtractAll(r, "extracted", opts...)
		if !errors.Is(err, errors.ErrUnsupported) {
			t.Fatalf("ExtractAll error = %v, want errors.ErrUnsupported", err)
		}
		if errors.Is(err, archives.ErrUnsafePath) || errors.Is(err, archives.ErrExtractLimit) {
			t.Fatalf("ExtractAll returned a native extraction error: %v", err)
		}
	}

	entries, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "package/README.md" {
		t.Fatalf("List = %v, want package/README.md", entries)
	}
	rc, err := r.Extract("package/README.md")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatalf("Extract content = %q, want %q", got, content)
	}
}
