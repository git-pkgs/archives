package archives

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"testing"
	"testing/iotest"

	"github.com/ulikunitz/xz"
)

type xzTestBlock struct {
	dictCode byte
	lzma2    []byte // LZMA2 chunks ending with the 0x00 end marker
	payload  []byte // what lzma2 decodes to, for the CRC32 check
}

// xzTestStream frames blocks as one xz stream with CRC32 checks, so a test
// can set the dictionary each block header declares.
func xzTestStream(blocks ...xzTestBlock) []byte {
	flags := []byte{0, 1}
	out := append([]byte{0xFD, '7', 'z', 'X', 'Z', 0}, flags...)
	out = binary.LittleEndian.AppendUint32(out, crc32.ChecksumIEEE(flags))
	index := binary.AppendUvarint([]byte{0}, uint64(len(blocks)))
	for _, b := range blocks {
		header := []byte{2, 0, 0x21, 1, b.dictCode, 0, 0, 0}
		header = binary.LittleEndian.AppendUint32(header, crc32.ChecksumIEEE(header))
		out = append(out, header...)
		out = append(out, b.lzma2...)
		out = append(out, make([]byte, (4-len(b.lzma2)%4)%4)...)
		out = binary.LittleEndian.AppendUint32(out, crc32.ChecksumIEEE(b.payload))
		index = binary.AppendUvarint(index, uint64(len(header)+len(b.lzma2)+4))
		index = binary.AppendUvarint(index, uint64(len(b.payload)))
	}
	index = append(index, make([]byte, (4-len(index)%4)%4)...)
	index = binary.LittleEndian.AppendUint32(index, crc32.ChecksumIEEE(index))
	out = append(out, index...)
	footer := binary.LittleEndian.AppendUint32(nil, uint32(len(index)/4-1))
	footer = append(footer, flags...)
	out = binary.LittleEndian.AppendUint32(out, crc32.ChecksumIEEE(footer))
	out = append(out, footer...)
	return append(out, 'Y', 'Z')
}

// xzStoredLZMA2 stores p in uncompressed LZMA2 chunks.
func xzStoredLZMA2(p []byte) []byte {
	var out []byte
	control := byte(1) // the first chunk resets the dictionary
	for len(p) > 0 {
		n := min(len(p), 1<<16)
		out = append(out, control, byte((n-1)>>8), byte(n-1))
		out = append(out, p[:n]...)
		p = p[n:]
		control = 2
	}
	return append(out, 0)
}

// xzLZMAChunk returns the single LZMA chunk ulikunitz/xz writes for p.
func xzLZMAChunk(t *testing.T, p []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := xz.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(p)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	data := raw[12+(int(raw[12])+1)*4:]
	if data[0] < 0xC0 {
		t.Fatalf("chunk control %#x, want an LZMA chunk with properties", data[0])
	}
	n := 6 + (int(data[3])<<8 | int(data[4])) + 1
	if data[n] != 0 {
		t.Fatalf("writer produced more than one chunk")
	}
	return append([]byte(nil), data[:n]...)
}

func TestXZDictGuardLimit(t *testing.T) {
	for _, tc := range []struct {
		code byte
		dict int64
		ok   bool
	}{
		{0, 4 << 10, true},
		{28, 64 << 20, true},
		{29, 96 << 20, false},
		{40, 1<<32 - 1, false},
	} {
		data := xzTestStream(xzTestBlock{tc.code, xzStoredLZMA2(zipMethodPayload), zipMethodPayload})
		if got := xzBlockHeaderDict(data[12:]); got != tc.dict {
			t.Fatalf("code %d: header dict %d, want %d", tc.code, got, tc.dict)
		}
		for name, src := range map[string]io.Reader{
			"whole":    bytes.NewReader(data),
			"one byte": iotest.OneByteReader(bytes.NewReader(data)),
		} {
			got, err := io.ReadAll(&xzDictGuard{r: src})
			if tc.ok {
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("code %d %s: %d of %d bytes, %v", tc.code, name, len(got), len(data), err)
				}
				continue
			}
			if !errors.Is(err, ErrDecompressLimit) {
				t.Fatalf("code %d %s: %v, want ErrDecompressLimit", tc.code, name, err)
			}
			if len(got) >= 24 {
				t.Fatalf("code %d %s: returned %d bytes, the header ends at 24", tc.code, name, len(got))
			}
		}
	}
}

// A header past the guard's first buffer, after the buffer has been
// compacted, is found at the right offset; a clean one decodes.
func TestXZDictGuardLaterBlock(t *testing.T) {
	big := bytes.Repeat(zipMethodPayload, 80) // 100 KB, two stored chunks
	for _, code := range []byte{0, 29} {
		data := xzTestStream(
			xzTestBlock{0, xzStoredLZMA2(big), big},
			xzTestBlock{code, xzStoredLZMA2(zipMethodPayload), zipMethodPayload},
		)
		for name, src := range map[string]io.Reader{
			"whole":    bytes.NewReader(data),
			"one byte": iotest.OneByteReader(bytes.NewReader(data)),
		} {
			r, err := newXZReader(src)
			if err == nil {
				var got []byte
				got, err = io.ReadAll(r)
				if err == nil && !bytes.Equal(got, append(append([]byte(nil), big...), zipMethodPayload...)) {
					t.Fatalf("code %d %s: wrong output", code, name)
				}
			}
			if code == 0 && err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if code != 0 && !errors.Is(err, ErrDecompressLimit) {
				t.Fatalf("code %d %s: %v, want ErrDecompressLimit", code, name, err)
			}
		}
	}
}

// The decoder does not check that an LZMA chunk's packed bytes are all
// consumed. A chunk that declares more packed bytes than its data uses ends
// early for the decoder, which then reads the rest as the end marker, the
// check and a second block header. A guard that skipped by packed size
// would treat that header as chunk data.
func TestXZDictGuardBlockHiddenInChunk(t *testing.T) {
	chunk := xzLZMAChunk(t, zipMethodPayload)
	for _, code := range []byte{0, 30} {
		data := xzTestStream(
			xzTestBlock{0, append(append([]byte(nil), chunk...), 0), zipMethodPayload},
			xzTestBlock{code, xzStoredLZMA2([]byte("second")), []byte("second")},
		)
		packedStart := 12 + 12 + 6
		secondHeaderEnd := 12 + 12 + len(chunk) + 1
		secondHeaderEnd += (4-(len(chunk)+1)%4)%4 + 4 + 12
		packed := secondHeaderEnd - packedStart
		data[12+12+3], data[12+12+4] = byte((packed-1)>>8), byte(packed-1)

		r, err := xz.NewReader(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if code == 0 {
			got, err := io.ReadAll(r)
			if err != nil || string(got) != string(zipMethodPayload)+"second" {
				t.Fatalf("decoder did not read the hidden block: %v", err)
			}
		}

		r, err = newXZReader(bytes.NewReader(data))
		if err == nil {
			_, err = io.ReadAll(r)
		}
		if code == 0 && err != nil {
			t.Fatal(err)
		}
		if code != 0 && !errors.Is(err, ErrDecompressLimit) {
			t.Fatalf("hidden block with code %d: %v, want ErrDecompressLimit", code, err)
		}
	}
}

func zipWithLargeXZDict(t *testing.T) []byte {
	t.Helper()
	member := xzTestStream(xzTestBlock{30, xzStoredLZMA2(zipMethodPayload), zipMethodPayload})
	return zipWithRawMember(t, "data.txt", zipMethodXZ, member, len(zipMethodPayload))
}

func TestZipXZMemberDictLimit(t *testing.T) {
	data := zipWithLargeXZDict(t)
	r, err := OpenBytes("test.zip", data)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := r.Extract("data.txt")
	if err == nil {
		_, err = io.ReadAll(rc)
		_ = rc.Close()
	}
	if !errors.Is(err, ErrDecompressLimit) {
		t.Fatalf("Extract: %v, want ErrDecompressLimit", err)
	}

	// Next discards an unread member by decoding it.
	stream, err := OpenStreamBytes("test.zip", data, StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if _, err := stream.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Next(); !errors.Is(err, ErrDecompressLimit) {
		t.Fatalf("second Next: %v, want ErrDecompressLimit", err)
	}
}

// condaWithOuterMethod compresses the outer pkg member with method instead
// of storing it; info stays stored.
func condaWithOuterMethod(t *testing.T, method uint16) []byte {
	t.Helper()
	pkg := writeTarZst(t, map[string]string{"lib/x.so": "content"})
	info := writeTarZst(t, map[string]string{"info/index.json": `{"name":"x"}`})

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, m := range []struct {
		name   string
		method uint16
		data   []byte
	}{
		{"pkg-x-1.tar.zst", method, pkg},
		{"info-x-1.tar.zst", zip.Store, info},
	} {
		body := m.data
		if m.method != zip.Store {
			body = compressForZip(t, m.method, m.data)
		}
		w, err := zw.CreateRaw(&zip.FileHeader{
			Name:               m.name,
			Method:             m.method,
			CRC32:              crc32.ChecksumIEEE(m.data),
			CompressedSize64:   uint64(len(body)),
			UncompressedSize64: uint64(len(m.data)),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var condaOuterMethods = []struct {
	name   string
	method uint16
}{
	{"zstd", zipMethodZstd},
	{"xz", zipMethodXZ},
}

func TestCondaCompressedOuterMemberOpenBytes(t *testing.T) {
	for _, tc := range condaOuterMethods {
		t.Run(tc.name, func(t *testing.T) {
			r, err := OpenBytes("x-1.conda", condaWithOuterMethod(t, tc.method))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			for path, want := range map[string]string{"lib/x.so": "content", "info/index.json": `{"name":"x"}`} {
				rc, err := r.Extract(path)
				if err != nil {
					t.Fatalf("Extract %s: %v", path, err)
				}
				got, err := io.ReadAll(rc)
				_ = rc.Close()
				if err != nil || string(got) != want {
					t.Fatalf("%s = %q, %v", path, got, err)
				}
			}
		})
	}
}

func TestCondaCompressedOuterMemberOpenStreamBytes(t *testing.T) {
	for _, tc := range condaOuterMethods {
		t.Run(tc.name, func(t *testing.T) {
			stream, err := OpenStreamBytes("x-1.conda", condaWithOuterMethod(t, tc.method), StreamOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stream.Close() }()
			got := map[string]string{}
			for {
				entry, err := stream.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				content, err := io.ReadAll(stream)
				if err != nil {
					t.Fatal(err)
				}
				got[entry.Path] = string(content)
			}
			if got["lib/x.so"] != "content" || got["info/index.json"] != `{"name":"x"}` {
				t.Fatalf("entries %v", got)
			}
		})
	}
}
