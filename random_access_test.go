package archives

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
			data = withZIP64Count(data, 2)
		}
		b, err := OpenBrowserReaderAt("test.zip", bytes.NewReader(data), int64(len(data)), BrowseOptions{})
		if err != nil {
			t.Fatal(err)
		}
		assertBrowsedBody(t, b, "root/small", "hello")
	}
}

// withZIP64Count adds a ZIP64 end record and locator to a ZIP that has no
// archive comment. The ZIP64 record advertises records entries.
func withZIP64Count(data []byte, records uint64) []byte {
	end := len(data) - zipDirectoryEndLen
	record := make([]byte, zipDirectory64EndLen+zipDirectory64LocatorLen)
	binary.LittleEndian.PutUint32(record, zipDirectory64EndSignature)
	binary.LittleEndian.PutUint64(record[4:], 44)
	binary.LittleEndian.PutUint64(record[24:], records)
	binary.LittleEndian.PutUint64(record[32:], records)
	binary.LittleEndian.PutUint64(record[40:], uint64(binary.LittleEndian.Uint32(data[end+12:])))
	binary.LittleEndian.PutUint64(record[48:], uint64(binary.LittleEndian.Uint32(data[end+16:])))
	binary.LittleEndian.PutUint32(record[56:], zipDirectory64LocSignature)
	binary.LittleEndian.PutUint64(record[64:], uint64(end))
	binary.LittleEndian.PutUint32(record[72:], 1)
	out := append(append(data[:end:end], record...), data[end:]...)
	binary.LittleEndian.PutUint16(out[len(out)-12:], zipDirectoryRecords64Marker)
	return out
}

// zipDirectoryStartForTest returns the central directory offset of data.
func zipDirectoryStartForTest(t *testing.T, data []byte) int {
	t.Helper()
	start, _, ok, err := zipCentralDirectoryBounds(bytes.NewReader(data), int64(len(data)))
	if err != nil || !ok {
		t.Fatalf("directory bounds: ok=%v err=%v", ok, err)
	}
	return int(start)
}

// wrappedCountZIP has a classic end record whose 16-bit count has wrapped.
func wrappedCountZIP(t *testing.T, entries int) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for i := range entries {
		if _, err := writer.CreateHeader(&zip.FileHeader{Name: strconv.Itoa(i), Method: zip.Store}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	start, end, ok, err := zipCentralDirectoryBounds(bytes.NewReader(data), int64(len(data)))
	if err != nil || !ok {
		t.Fatalf("directory bounds: ok=%v err=%v", ok, err)
	}
	record := make([]byte, zipDirectoryEndLen)
	binary.LittleEndian.PutUint32(record, zipDirectoryEndSignature)
	binary.LittleEndian.PutUint16(record[8:], uint16(entries))
	binary.LittleEndian.PutUint16(record[10:], uint16(entries))
	binary.LittleEndian.PutUint32(record[12:], uint32(end-start))
	binary.LittleEndian.PutUint32(record[16:], uint32(start))
	return append(data[:end:end], record...)
}

func streamContents(stream *Stream, openErr error) (string, error) {
	if openErr != nil {
		return "", openErr
	}
	defer func() { _ = stream.Close() }()
	var out strings.Builder
	for {
		entry, err := stream.Next()
		if err == io.EOF {
			return out.String(), nil
		}
		if err != nil {
			return out.String(), err
		}
		body, err := io.ReadAll(stream)
		if err != nil {
			return out.String(), err
		}
		fmt.Fprintf(&out, "%s=%x\n", entry.Path, sha256.Sum256(body))
	}
}

func TestZIPParserParity(t *testing.T) {
	valid := randomAccessZIP(t, 16)
	edit := func(change func([]byte)) func(*testing.T) []byte {
		return func(t *testing.T) []byte {
			t.Helper()
			data := bytes.Clone(valid)
			change(data)
			return data
		}
	}
	endCount := func(count uint16) func(*testing.T) []byte {
		return edit(func(data []byte) { binary.LittleEndian.PutUint16(data[len(data)-12:], count) })
	}
	for _, tc := range []struct {
		name       string
		build      func(*testing.T) []byte
		maxEntries int
		wantErr    error
		wantOpen   bool
	}{
		{name: "valid", build: edit(func([]byte) {}), wantOpen: true},
		{name: "wrapped count", build: func(t *testing.T) []byte { t.Helper(); return wrappedCountZIP(t, 70_000) }, wantOpen: true},
		{name: "wrapped count over limit", build: func(t *testing.T) []byte { t.Helper(); return wrappedCountZIP(t, 70_000) }, maxEntries: 69_999, wantErr: ErrEntryLimit},
		{name: "headers over limit", build: edit(func([]byte) {}), maxEntries: 1, wantErr: ErrEntryLimit},
		// archive/zip compares the advertised count modulo 65536, so these
		// fixtures disagree with the headers but archive/zip still opens them.
		{name: "underreported count", build: func(t *testing.T) []byte { t.Helper(); return wrappedCountZIP(t, 65_537) }, maxEntries: 65_537, wantOpen: true},
		{name: "advertised count over limit", build: func(*testing.T) []byte { return withZIP64Count(bytes.Clone(valid), 2+65_536) }, maxEntries: 2, wantOpen: true},
		{name: "header length", build: func(t *testing.T) []byte {
			t.Helper()
			data := bytes.Clone(valid)
			binary.LittleEndian.PutUint16(data[zipDirectoryStartForTest(t, data)+28:], 65535)
			return data
		}},
		{name: "header signature", build: func(t *testing.T) []byte {
			t.Helper()
			data := bytes.Clone(valid)
			data[zipDirectoryStartForTest(t, data)] = 0
			return data
		}},
		{name: "second header signature", build: func(t *testing.T) []byte {
			t.Helper()
			data := bytes.Clone(valid)
			start := zipDirectoryStartForTest(t, data)
			first := zipDirectoryHeaderLen + int(binary.LittleEndian.Uint16(data[start+28:]))
			data[start+first] = 0
			return data
		}, maxEntries: 1},
		{name: "no end record", build: edit(func(data []byte) { data[len(data)-zipDirectoryEndLen] = 0 })},
		{name: "missing zip64 locator", build: endCount(zipDirectoryRecords64Marker)},
		{name: "directory size past start", build: edit(func(data []byte) {
			binary.LittleEndian.PutUint32(data[len(data)-10:], uint32(len(data)))
		})},
		{name: "truncated", build: func(*testing.T) []byte { return valid[:12] }},
		{name: "empty", build: func(*testing.T) []byte { return nil }},
		{name: "executable prefix", build: func(*testing.T) []byte { return append([]byte("executable prefix"), valid...) }, wantOpen: true},
		{name: "comment past short window", build: func(*testing.T) []byte {
			return withZIPComment(valid, 4*zipDirectoryEndShortWindow)
		}, maxEntries: 1, wantErr: ErrEntryLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := tc.build(t)
			options := StreamOptions{MaxEntries: tc.maxEntries}
			bytesStream, bytesOpenErr := OpenStreamBytes("test.zip", data, options)
			bytesOut, bytesErr := streamContents(bytesStream, bytesOpenErr)
			randomStream, randomOpenErr := OpenStreamReaderAt("test.zip", bytes.NewReader(data), int64(len(data)), options)
			randomOut, randomErr := streamContents(randomStream, randomOpenErr)

			if (bytesOpenErr == nil) != (randomOpenErr == nil) {
				t.Fatalf("open errors differ: bytes=%v reader-at=%v", bytesOpenErr, randomOpenErr)
			}
			if fmt.Sprint(bytesErr) != fmt.Sprint(randomErr) {
				t.Fatalf("errors differ: bytes=%v reader-at=%v", bytesErr, randomErr)
			}
			if bytesOut != randomOut {
				t.Fatalf("entries differ:\nbytes:\n%s\nreader-at:\n%s", bytesOut, randomOut)
			}
			if tc.wantErr != nil && !errors.Is(randomErr, tc.wantErr) {
				t.Fatalf("error = %v, want %v", randomErr, tc.wantErr)
			}
			if tc.wantOpen && randomErr != nil {
				t.Fatalf("error = %v, want success", randomErr)
			}
		})
	}
}

func withZIPComment(data []byte, length int) []byte {
	data = bytes.Clone(data)
	binary.LittleEndian.PutUint16(data[len(data)-2:], uint16(length))
	return append(data, bytes.Repeat([]byte("c"), length)...)
}

// The open path also checks len(reader.File), so only a direct parser call
// shows that the wide search found the end record.
func TestZIPEntryCountSearchesPastShortWindow(t *testing.T) {
	data := withZIPComment(randomAccessZIP(t, 16), 4*zipDirectoryEndShortWindow)
	err := checkZipEntryCountLimitContext(context.Background(), bytes.NewReader(data), int64(len(data)), 1)
	if !errors.Is(err, ErrEntryLimit) {
		t.Fatalf("error = %v, want %v", err, ErrEntryLimit)
	}
}

func TestZIPEntryCountBuffersDirectoryReads(t *testing.T) {
	const entries = 5000
	data := wrappedCountZIP(t, entries)
	input := &countedReaderAt{Reader: bytes.NewReader(data)}
	if err := checkZipEntryCountLimitContext(context.Background(), input, int64(len(data)), entries); err != nil {
		t.Fatal(err)
	}
	if input.calls >= entries/10 {
		t.Fatalf("ReadAt calls = %d for %d entries", input.calls, entries)
	}
}

func TestReaderAtSourceShorterThanSize(t *testing.T) {
	data := randomAccessZIP(t, 16)
	if _, err := OpenStreamReaderAt("test.zip", bytes.NewReader(data), int64(len(data))+1, StreamOptions{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error = %v, want %v", err, io.ErrUnexpectedEOF)
	}
}
