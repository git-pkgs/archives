package archives

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func randomAccessZIP(t testing.TB, payloadSize int) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for _, entry := range []struct {
		name string
		data []byte
	}{{"root/large", make([]byte, payloadSize)}, {"root/small", []byte("hello")}} {
		out, err := writer.CreateHeader(&zip.FileHeader{Name: entry.name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := out.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type countedReaderAt struct {
	*bytes.Reader
	reads, calls, total int
}

func (r *countedReaderAt) Read(p []byte) (int, error) { r.reads++; return r.Reader.Read(p) }
func (r *countedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.calls++
	n, err := r.Reader.ReadAt(p, off)
	r.total += n
	return n, err
}

func TestBrowserReaderAt(t *testing.T) {
	data := randomAccessZIP(t, 2<<20)
	input := &countedReaderAt{Reader: bytes.NewReader(data)}
	browser, err := OpenBrowserReaderAt("test.zip", input, int64(len(data)), BrowseOptions{DetectRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		files, err := browser.List()
		if err != nil || len(files) != 2 || files[0].Path != "large" {
			t.Fatalf("List = %+v, %v", files, err)
		}
		assertBrowsedBody(t, browser, "small", "hello")
	}
	if input.reads != 0 || input.calls == 0 || input.total >= len(data)/2 {
		t.Fatalf("Read=%d ReadAt=%d bytes=%d archive=%d", input.reads, input.calls, input.total, len(data))
	}
}

func TestBrowserReaderAtFile(t *testing.T) {
	data := randomAccessZIP(t, 128)
	name := filepath.Join(t.TempDir(), "test.zip")
	if err := os.WriteFile(name, data, 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	browser, err := OpenBrowserReaderAt("test.zip", input, int64(len(data)), BrowseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertBrowsedBody(t, browser, "root/small", "hello")
	if _, err := input.Stat(); err != nil {
		t.Fatalf("caller input closed: %v", err)
	}
}

func TestStreamReaderAtFormats(t *testing.T) {
	for _, name := range []string{"test.zip", "test.conda", "renamed"} {
		t.Run(name, func(t *testing.T) {
			format := name
			if name == "renamed" {
				format = "test.zip"
			}
			data := streamTestArchive(t, format, browseTar(t, "root/a", "root/b"))
			stream, err := OpenStreamReaderAt(name, bytes.NewReader(data), int64(len(data)), StreamOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stream.Close() }()
			count := 0
			for {
				_, err := stream.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if _, err := io.Copy(io.Discard, stream); err != nil {
					t.Fatal(err)
				}
				count++
			}
			if count != 2 {
				t.Fatalf("entries = %d", count)
			}
		})
	}
}

func TestReaderAtLimitsAndCancellation(t *testing.T) {
	data := randomAccessZIP(t, 128)
	for _, tc := range []struct {
		name    string
		options StreamOptions
		want    error
	}{
		{"input", StreamOptions{MaxInputBytes: int64(len(data) - 1)}, ErrInputLimit},
		{"entries", StreamOptions{MaxEntries: 1}, ErrEntryLimit},
		{"entry bytes", StreamOptions{MaxEntryBytes: 127}, ErrEntrySizeLimit},
		{"expanded", StreamOptions{MaxExpandedBytes: 128}, ErrDecompressLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := OpenBrowserReaderAt("test.zip", bytes.NewReader(data), int64(len(data)), BrowseOptions{Limits: tc.options})
			if err == nil {
				_, err = b.List()
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	b, err := OpenBrowserReaderAtContext(ctx, "test.zip", bytes.NewReader(data), int64(len(data)), BrowseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	body, err := b.Extract("root/small")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	cancel()
	if _, err = body.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("read error = %v", err)
	}
	if _, err = b.List(); !errors.Is(err, context.Canceled) {
		t.Fatalf("List error = %v", err)
	}
	if _, err = OpenStreamReaderAtContext(ctx, "test.zip", bytes.NewReader(data), int64(len(data)), StreamOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("open error = %v", err)
	}
}

func TestReaderAtMalformedDirectory(t *testing.T) {
	valid := randomAccessZIP(t, 16)
	for _, tc := range []struct {
		name   string
		change func([]byte)
		want   error
	}{
		{"underreported count", func(data []byte) { binary.LittleEndian.PutUint16(data[len(data)-12:], 1) }, zip.ErrFormat},
		{"advertised count", func(data []byte) { binary.LittleEndian.PutUint16(data[len(data)-12:], 40000) }, ErrEntryLimit},
		{"header length", func(data []byte) {
			start, _, _ := zipCentralDirectoryBounds(data)
			binary.LittleEndian.PutUint16(data[start+28:], 65535)
		}, zip.ErrFormat},
		{"signature", func(data []byte) { start, _, _ := zipCentralDirectoryBounds(data); data[start] = 0 }, zip.ErrFormat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := bytes.Clone(valid)
			tc.change(data)
			if _, err := OpenStreamReaderAt("test.zip", bytes.NewReader(data), int64(len(data)), StreamOptions{MaxEntries: 2}); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
	for _, size := range []int64{-1, 0, 12} {
		if _, err := OpenStreamReaderAt("test.zip", bytes.NewReader(valid), size, StreamOptions{}); err == nil {
			t.Fatalf("accepted size %d", size)
		}
	}
	if _, err := OpenStreamReaderAt("test.zip", bytes.NewReader(valid), int64(len(valid)), StreamOptions{MaxEntries: -1}); err == nil {
		t.Fatal("accepted negative limit")
	}
	if _, err := OpenStreamReaderAt("test.tar", bytes.NewReader(valid), int64(len(valid)), StreamOptions{}); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("format error = %v", err)
	}
}

func BenchmarkBrowserReaderAt(b *testing.B) {
	data := randomAccessZIP(b, 16<<20)
	for _, random := range []bool{false, true} {
		name := "buffered"
		if random {
			name = "reader-at"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var browser *Browser
				var err error
				if random {
					browser, err = OpenBrowserReaderAt("test.zip", bytes.NewReader(data), int64(len(data)), BrowseOptions{})
				} else {
					browser, err = OpenBrowser("test.zip", bytes.NewReader(data), nil, BrowseOptions{})
				}
				if err != nil {
					b.Fatal(err)
				}
				if _, err = browser.List(); err != nil {
					b.Fatal(err)
				}
				file, err := browser.Extract("root/small")
				if err != nil {
					b.Fatal(err)
				}
				if _, err = io.Copy(io.Discard, file); err != nil {
					b.Fatal(err)
				}
				if err = file.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestReaderAtZIP64AndPrefix(t *testing.T) {
	original := randomAccessZIP(t, 32)
	for _, prefix := range []bool{false, true} {
		data := bytes.Clone(original)
		if prefix {
			data = append([]byte("executable prefix"), data...)
		} else {
			end := len(data) - zipDirectoryEndLen
			record := make([]byte, zipDirectory64EndLen+zipDirectory64LocatorLen)
			binary.LittleEndian.PutUint32(record, zipDirectory64EndSignature)
			binary.LittleEndian.PutUint64(record[4:], 44)
			binary.LittleEndian.PutUint64(record[24:], 2)
			binary.LittleEndian.PutUint64(record[32:], 2)
			binary.LittleEndian.PutUint64(record[40:], uint64(binary.LittleEndian.Uint32(data[end+12:])))
			binary.LittleEndian.PutUint64(record[48:], uint64(binary.LittleEndian.Uint32(data[end+16:])))
			binary.LittleEndian.PutUint32(record[56:], zipDirectory64LocSignature)
			binary.LittleEndian.PutUint64(record[64:], uint64(end))
			binary.LittleEndian.PutUint32(record[72:], 1)
			data = append(append(data[:end:end], record...), data[end:]...)
			binary.LittleEndian.PutUint16(data[len(data)-12:], zipDirectoryRecords64Marker)
		}
		b, err := OpenBrowserReaderAt("test.zip", bytes.NewReader(data), int64(len(data)), BrowseOptions{})
		if err != nil {
			t.Fatal(err)
		}
		assertBrowsedBody(t, b, "root/small", "hello")
	}
}
