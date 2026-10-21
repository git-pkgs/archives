package archives

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"testing"
)

const singleFilePayload = "certificate content\n"

func singleFileFormats(t *testing.T) map[string][]byte {
	t.Helper()
	bzip, err := base64.StdEncoding.DecodeString("QlpoOTFBWSZTWSxCGs8AAAJRgAAQQAArIZQAIAAxANNNBCNtUzSaFoISuvM63zfi7kinChIFiENZ4A==")
	if err != nil {
		t.Fatal(err)
	}
	return map[string][]byte{
		"gz": gzipTar(t, []byte(singleFilePayload)), "bz2": bzip,
		"xz":  compressTar(t, "test.tar.xz", []byte(singleFilePayload)),
		"zst": compressTar(t, "test.tar.zst", []byte(singleFilePayload)),
	}
}

func TestCompressedSingleFileReaders(t *testing.T) {
	for format, data := range singleFileFormats(t) {
		for _, filename := range []string{"bundle.pem." + format, "renamed"} {
			for _, buffered := range []bool{false, true} {
				var r Reader
				var err error
				if buffered {
					r, err = OpenBytes(filename, data)
				} else {
					r, err = Open(filename, bytes.NewReader(data))
				}
				if err != nil {
					t.Fatal(err)
				}
				files, err := r.List()
				if err != nil || len(files) != 1 || files[0].Path != "content" || files[0].Size != int64(len(singleFilePayload)) {
					t.Fatalf("%s: %+v %v", filename, files, err)
				}
				assertTarPayload(t, r, "content", []byte(singleFilePayload))
				hash, err := r.Hash("SHA256")
				if err != nil || hash != fmt.Sprintf("%x", sha256.Sum256(data)) {
					t.Fatalf("compressed hash: %s %v", hash, err)
				}
				_ = r.Close()
			}
		}
	}
}

func TestCompressedSingleFileStreams(t *testing.T) {
	for format, data := range singleFileFormats(t) {
		for _, read := range []bool{false, true} {
			s, err := OpenStream("bundle."+format, bytes.NewReader(data), StreamOptions{})
			if err != nil {
				t.Fatal(err)
			}
			entry, err := s.Next()
			if err != nil || entry.Path != "content" || entry.Size != -1 {
				t.Fatalf("%s: %+v %v", format, entry, err)
			}
			if read {
				body, err := io.ReadAll(s)
				if err != nil || string(body) != singleFilePayload {
					t.Fatalf("%s: %q %v", format, body, err)
				}
			}
			if _, err := s.Next(); err != io.EOF {
				t.Fatal(err)
			}
			_ = s.Close()
		}
	}
}

func TestCompressedSingleFileZeroPrefixAndEmptyContent(t *testing.T) {
	for _, payload := range [][]byte{nil, append(make([]byte, 1024), []byte(singleFilePayload)...)} {
		s, err := OpenStreamBytes("renamed", gzipTar(t, payload), StreamOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if entry, err := s.Next(); err != nil || entry.Path != "content" {
			t.Fatalf("%+v %v", entry, err)
		}
		body, err := io.ReadAll(s)
		if err != nil || !bytes.Equal(body, payload) {
			t.Fatalf("payload mismatch: %v", err)
		}
		_ = s.Close()
	}
}

func TestCompressedSingleFileLimits(t *testing.T) {
	for format, data := range singleFileFormats(t) {
		for _, test := range []struct {
			options StreamOptions
			want    error
		}{
			{StreamOptions{MaxEntryBytes: 5}, ErrEntrySizeLimit},
			{StreamOptions{MaxExpandedBytes: 5}, ErrDecompressLimit},
			{StreamOptions{MaxInputBytes: int64(len(data) - 1)}, ErrInputLimit},
			{StreamOptions{MaxEntryBytes: int64(len(singleFilePayload)), MaxExpandedBytes: int64(len(singleFilePayload))}, nil},
		} {
			for _, read := range []bool{false, true} {
				s, err := OpenStreamBytes("renamed", data, test.options)
				if err == nil {
					err = consumeStream(s, read)
					_ = s.Close()
				}
				if !errors.Is(err, test.want) {
					t.Fatalf("%s read=%v: %v, want %v", format, read, err, test.want)
				}
			}
		}
	}
}

func TestCompressedSingleFileCancellationAndTrailer(t *testing.T) {
	data := gzipTar(t, bytes.Repeat([]byte(singleFilePayload), 1000))
	ctx, cancel := context.WithCancel(context.Background())
	s, err := OpenStreamBytesContext(ctx, "data.gz", data, StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next(); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := io.ReadAll(s); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_ = s.Close()
	data[len(data)-8] ^= 1
	s, err = OpenStreamBytes("bad.gz", data, StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := consumeStream(s, false); err == nil {
		t.Fatal("corrupt gzip trailer accepted")
	}
	_ = s.Close()
}

func TestCompressedSingleFileBrowser(t *testing.T) {
	data := gzipTar(t, []byte(singleFilePayload))
	browser, err := OpenBrowser("bundle.gz", bytes.NewReader(data), func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}, BrowseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	files, err := browser.List()
	if err != nil || len(files) != 1 || files[0].Path != "content" || files[0].Size != -1 {
		t.Fatalf("%+v %v", files, err)
	}
	reader, err := browser.Extract("content")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	body, err := io.ReadAll(reader)
	if err != nil || string(body) != singleFilePayload {
		t.Fatalf("%q %v", body, err)
	}
}
