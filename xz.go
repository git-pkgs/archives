package archives

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"
)

// maxXZDictSize is the largest LZMA2 dictionary an xz block may declare.
// ulikunitz/xz allocates the declared size when it opens a block, up to
// 4 GiB; xz -9 uses 64 MiB.
const maxXZDictSize = 64 << 20

// xzMaxBlockHeader is the size of the largest xz block header,
// (0xFF + 1) * 4 bytes.
const xzMaxBlockHeader = 1024

// newXZReader opens an xz stream and fails, before the decoder allocates
// anything for it, on a block that declares a dictionary larger than
// maxXZDictSize.
func newXZReader(r io.Reader) (*xz.Reader, error) {
	return xz.NewReader(&xzDictGuard{r: r})
}

// xzDictGuard checks every 4-aligned offset instead of following the LZMA2
// chunks, because ulikunitz/xz does not verify that a chunk's packed bytes
// are all consumed.
type xzDictGuard struct {
	r      io.Reader
	buf    []byte
	start  int   // buf[start:end] has been checked and not returned yet
	end    int   // buf[end:] waits for the header that could start in it
	off    int64 // stream offset of buf[0]
	srcErr error // error from r, returned once buf is drained
	err    error
}

func (g *xzDictGuard) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for g.start == g.end {
		if g.err != nil {
			return 0, g.err
		}
		if g.srcErr != nil {
			return 0, g.srcErr
		}
		g.advance()
	}
	n := copy(p, g.buf[g.start:g.end])
	g.start += n
	return n, nil
}

// advance reads more from r and checks every 4-aligned header start in the
// bytes that can now be returned.
func (g *xzDictGuard) advance() {
	if g.start > 0 {
		n := copy(g.buf, g.buf[g.start:])
		g.buf = g.buf[:n]
		g.off += int64(g.start)
		g.end -= g.start
		g.start = 0
	}
	if g.buf == nil {
		g.buf = make([]byte, 0, 32<<10)
	}
	n, err := g.r.Read(g.buf[len(g.buf):cap(g.buf)])
	g.buf = g.buf[:len(g.buf)+n]
	if err != nil {
		g.srcErr = err
	}

	ready := len(g.buf)
	if g.srcErr == nil {
		ready -= xzMaxBlockHeader - 1
	}
	i := g.end + int((4-(g.off+int64(g.end))%4)%4)
	for ; i < ready; i += 4 {
		if i+1 < len(g.buf) && g.buf[i+1]&0x3F != 0 {
			continue // see the flags check in xzBlockHeaderDict
		}
		if dict := xzBlockHeaderDict(g.buf[i:]); dict > maxXZDictSize {
			g.err = fmt.Errorf("%w: xz block at offset %d declares a %d-byte dictionary, limit is %d",
				ErrDecompressLimit, g.off+int64(i), dict, maxXZDictSize)
			return
		}
	}
	g.end = max(g.end, ready)
}

// xzBlockHeaderDict returns the dictionary size declared by the xz block
// header at the start of b, or 0 if b does not start with a complete header
// the decoder would accept. ulikunitz/xz supports exactly one filter, LZMA2.
func xzBlockHeaderDict(b []byte) int64 {
	if len(b) < 2 || b[0] == 0 {
		return 0
	}
	n := (int(b[0]) + 1) * 4
	if len(b) < n {
		return 0
	}
	flags := b[1]
	if flags&0x3F != 0 { // reserved bits, or more than one filter
		return 0
	}
	h := b[2 : n-4]
	for _, present := range []bool{flags&0x40 != 0, flags&0x80 != 0} {
		if !present {
			continue
		}
		_, k := binary.Uvarint(h)
		if k <= 0 {
			return 0
		}
		h = h[k:]
	}
	// The decoder reads the filter ID as a varint, so 0xA1 0x00 is 0x21 too.
	id, k := binary.Uvarint(h)
	if k <= 0 || id != 0x21 || len(h) < k+2 || h[k] != 1 {
		return 0
	}
	dict, err := lzma.DecodeDictCap(h[k+1])
	if err != nil {
		return 0
	}
	if crc32.ChecksumIEEE(b[:n-4]) != binary.LittleEndian.Uint32(b[n-4:n]) {
		return 0
	}
	return dict
}
