package archives

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"reflect"
	"testing"
)

func TestStreamFormats(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"source.zip", createTestZip()},
		{"source.tar", createTestTar()},
		{"source.tgz", createTestTarGz()},
		{"source.tar.bz2", createTestTarBz2(t)},
		{"source.tar.xz", createTestTarXz(t)},
		{"source.tar.zst", createTestTarZst(t)},
		{"source.gem", createTestGem()},
		{"source.conda", createTestConda(t)},
		{"alpine.apk", createTestTarGz()},
		{"android.apk", createTestZip()},
		{"artifact", createTestTarGz()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := OpenBytes(tc.name, tc.data)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			want, err := r.List()
			if err != nil {
				t.Fatal(err)
			}
			for _, buffered := range []bool{false, true} {
				var stream *Stream
				if buffered {
					stream, err = OpenStream(tc.name, bytes.NewBuffer(tc.data), StreamOptions{})
				} else {
					stream, err = OpenStreamBytes(tc.name, tc.data, StreamOptions{})
				}
				if err != nil {
					t.Fatal(err)
				}
				assertStreamMatches(t, stream, r, want)
			}
		})
	}
}

func assertStreamMatches(t *testing.T, stream *Stream, r Reader, want []FileInfo) {
	t.Helper()
	defer func() { _ = stream.Close() }()
	for _, info := range want {
		entry, err := stream.Next()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(entry.FileInfo, info) {
			t.Fatalf("metadata = %+v, want %+v", entry.FileInfo, info)
		}
		got, err := io.ReadAll(stream)
		if err != nil {
			t.Fatal(err)
		}
		if info.IsDir {
			if len(got) != 0 {
				t.Fatal("directory has content")
			}
			continue
		}
		body, err := r.Extract(info.Path)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(body)
		_ = body.Close()
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("%s: content differs, %v", info.Path, err)
		}
	}
	for range 2 {
		if _, err := stream.Next(); err != io.EOF {
			t.Fatalf("end: %v", err)
		}
	}
}

func TestStreamLimits(t *testing.T) {
	raw := tarWithPayloads(t, []byte("aaa"), []byte("bbbb"), nil)
	for _, name := range []string{"test.tar", "test.tar.gz", "test.tar.xz", "test.tar.zst", "test.zip", "test.gem", "test.conda"} {
		data := streamTestArchive(t, name, raw)
		for _, tc := range []struct {
			name string
			opts StreamOptions
			want error
		}{
			{"entry", StreamOptions{MaxEntryBytes: 3}, ErrEntrySizeLimit},
			{"total", StreamOptions{MaxExpandedBytes: 6}, ErrDecompressLimit},
			{"count", StreamOptions{MaxEntries: 2}, ErrEntryLimit},
			{"exact", StreamOptions{MaxExpandedBytes: 7, MaxEntryBytes: 4, MaxEntries: 3}, nil},
			{"input", StreamOptions{MaxInputBytes: int64(len(data) - 1)}, ErrInputLimit},
		} {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				for _, readBodies := range []bool{false, true} {
					s, err := OpenStreamBytes(name, data, tc.opts)
					if err == nil {
						err = consumeStream(s, readBodies)
						_ = s.Close()
					}
					if !errors.Is(err, tc.want) {
						t.Fatalf("read=%v: got %v, want %v", readBodies, err, tc.want)
					}
				}
			})
		}
	}
}

func consumeStream(s *Stream, read bool) error {
	for {
		_, err := s.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if read {
			if _, err := io.Copy(io.Discard, s); err != nil {
				return err
			}
		}
	}
}

func streamTestArchive(t testing.TB, name string, raw []byte) []byte {
	t.Helper()
	switch name {
	case "test.gem":
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		data := gzipTar(t, raw)
		if err := tw.WriteHeader(&tar.Header{Name: "data.tar.gz", Size: int64(len(data)), Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	case "test.zip", "test.conda":
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		if name == "test.conda" {
			w, err := zw.Create("pkg-test.tar.zst")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write(compressTar(t, "test.tar.zst", raw)); err != nil {
				t.Fatal(err)
			}
		} else {
			streamTestZipEntries(t, zw, raw)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	default:
		return compressTar(t, name, raw)
	}
}

func streamTestZipEntries(t testing.TB, zw *zip.Writer, raw []byte) {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		w, err := zw.Create(h.Name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(w, tr); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStreamRejectsHeaderBeforeBody(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "huge", Size: 1 << 40, Mode: 0o644, Format: tar.FormatGNU}); err != nil {
		t.Fatal(err)
	}
	input := &countStreamInput{Reader: bytes.NewReader(buf.Bytes())}
	s, err := OpenStream("huge.tar", input, StreamOptions{MaxEntryBytes: 8 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if input.bytes != 0 {
		t.Fatal("opened by reading input")
	}
	for range 2 {
		if _, err := s.Next(); !errors.Is(err, ErrEntrySizeLimit) {
			t.Fatalf("Next: %v", err)
		}
	}
	if _, err := s.Read(make([]byte, 1)); !errors.Is(err, ErrEntrySizeLimit) {
		t.Fatalf("Read after rejection: %v", err)
	}
	if input.bytes != 512 {
		t.Fatalf("read %d input bytes, want header only", input.bytes)
	}
}

type countStreamInput struct {
	io.Reader
	bytes  int
	closed bool
}

func (r *countStreamInput) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.bytes += n
	return n, err
}

func (r *countStreamInput) Close() error {
	r.closed = true
	return nil
}

func TestStreamLifecycle(t *testing.T) {
	input := &countStreamInput{Reader: bytes.NewReader(createTestTarGz())}
	s, err := OpenStream("source.tgz", input, StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("Read before Next: %v", err)
	}
	if _, err := s.Next(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if input.closed {
		t.Fatal("closed caller's input")
	}
	if _, err := s.Next(); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("Next after Close: %v", err)
	}
	if _, err := s.Read(make([]byte, 1)); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("Read after Close: %v", err)
	}
}

func TestStreamLinksAndDuplicatePaths(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	writeTarFile(t, tw, "run", "first", 0o755)
	writeTarFile(t, tw, "run", "second", 0o644)
	for _, h := range []*tar.Header{
		{Name: "soft", Linkname: "run", Typeflag: tar.TypeSymlink},
		{Name: "hard", Linkname: "run", Typeflag: tar.TypeLink},
	} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStreamBytes("links.tar", buf.Bytes(), StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for i, want := range []string{"first", "second", "", ""} {
		e, err := s.Next()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(s)
		if err != nil || string(body) != want {
			t.Fatalf("body %d: %q, %v", i, body, err)
		}
		if i == 0 && e.Mode&0o111 == 0 {
			t.Fatal("executable mode lost")
		}
		if i >= 2 && (e.Linkname != "run" || e.IsHardlink != (i == 3)) {
			t.Fatalf("link metadata: %+v", e)
		}
	}
}

func TestStreamTruncatedSkippedBody(t *testing.T) {
	raw := tarWithPayloads(t, make([]byte, 1024))[:1023]
	for _, name := range []string{"test.tar", "test.tar.gz", "test.gem", "test.conda"} {
		s, err := OpenStreamBytes(name, streamTestArchive(t, name, raw), StreamOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Next(); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Next(); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("%s: %v", name, err)
		}
		_ = s.Close()
	}
}

func TestStreamInputBounds(t *testing.T) {
	data := createTestZip()
	for _, delta := range []int{-1, 0, 1} {
		input := &countStreamInput{Reader: bytes.NewReader(data)}
		s, err := OpenStream("test.zip", input, StreamOptions{MaxInputBytes: int64(len(data) + delta)})
		switch {
		case delta < 0:
			if !errors.Is(err, ErrInputLimit) {
				t.Fatalf("input over limit: %v", err)
			}
		case err != nil:
			t.Fatal(err)
		default:
			_ = s.Close()
		}
		if input.bytes > len(data)+delta+1 {
			t.Fatal("read beyond input limit probe")
		}
	}
	for _, options := range []StreamOptions{{MaxInputBytes: -1}, {MaxEntryBytes: -1}, {MaxExpandedBytes: -1}, {MaxEntries: -1}} {
		if _, err := OpenStreamBytes("test.zip", data, options); err == nil {
			t.Fatalf("accepted negative limit: %+v", options)
		}
	}
}

func BenchmarkStreamTar(b *testing.B) {
	for _, count := range []int{1, 128} {
		payloads := make([][]byte, count)
		for i := range payloads {
			payloads[i] = make([]byte, 64<<10)
		}
		data := gzipTar(b, tarWithPayloads(b, payloads...))
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				s, err := OpenStreamBytes("source.tgz", data, StreamOptions{})
				if err != nil {
					b.Fatal(err)
				}
				if err := consumeStream(s, true); err != nil {
					b.Fatal(err)
				}
				_ = s.Close()
			}
		})
	}
}

func TestStreamPartialRead(t *testing.T) {
	raw := tarWithPayloads(t, []byte("first"), []byte("second"))
	for _, name := range []string{"test.tar", "test.tar.gz", "test.zip", "test.gem", "test.conda"} {
		s, err := OpenStreamBytes(name, streamTestArchive(t, name, raw), StreamOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Next(); err != nil {
			t.Fatal(err)
		}
		var prefix [2]byte
		if _, err := io.ReadFull(s, prefix[:]); err != nil || string(prefix[:]) != "fi" {
			t.Fatalf("partial read: %q, %v", prefix, err)
		}
		if _, err := s.Next(); err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(s)
		if err != nil || string(body) != "second" {
			t.Fatalf("%s second entry: %q, %v", name, body, err)
		}
		_ = s.Close()
	}
}

func TestStreamZipChecksumOnSkip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "file", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "payload"); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	i := bytes.Index(data, []byte("payload"))
	if i < 0 {
		t.Fatal("missing fixture payload")
	}
	data[i] ^= 1
	s, err := OpenStreamBytes("bad.zip", data, StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Next(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := s.Next(); !errors.Is(err, zip.ErrChecksum) {
			t.Fatalf("skipped corrupt entry: %v", err)
		}
	}
}

func TestStreamCondaCombinedLimits(t *testing.T) {
	data := createTestConda(t)
	for _, opts := range []StreamOptions{{MaxEntries: 4}, {MaxExpandedBytes: 30}} {
		s, err := OpenStreamBytes("test.conda", data, opts)
		if err != nil {
			t.Fatal(err)
		}
		err = consumeStream(s, false)
		_ = s.Close()
		want := ErrDecompressLimit
		if opts.MaxEntries != 0 {
			want = ErrEntryLimit
		}
		if !errors.Is(err, want) {
			t.Fatalf("combined member budget: %v, want %v", err, want)
		}
	}
}

func TestStreamUnsupportedAndMissingMembers(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"unknown", []byte("no archive")},
		{"bad.zip", []byte("no zip")},
		{"bad.tgz", []byte("no gzip")},
		{"bad.gem", createTestTar()},
		{"bad.conda", createTestZip()},
	} {
		s, err := OpenStreamBytes(tc.name, tc.data, StreamOptions{})
		if err == nil {
			err = consumeStream(s, false)
			_ = s.Close()
		}
		if err == nil {
			t.Fatalf("accepted %s", tc.name)
		}
	}
}
