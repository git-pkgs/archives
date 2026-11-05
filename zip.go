package archives

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/klauspost/compress/flate"
)

// zipUnixModeShift is the bit offset of the Unix st_mode field within a
// ZIP entry's external attributes word. Unix permissions are only present
// when the creator system in CreatorVersion is Unix or macOS; other creators
// may store unrelated data in the high word, and archive/zip.FileHeader.Mode
// then returns a synthesised 0666/0444 that callers should not treat as a
// stored mode.
const (
	zipUnixModeShift             = 16
	zipCreatorShift              = 8
	zipCreatorUnix               = 3
	zipCreatorMacOSX             = 19
	zipSignatureLen              = 4
	zipDirectoryHeaderLen        = 46
	zipDirectoryEndLen           = 22
	zipDirectory64EndLen         = 56
	zipDirectory64LocatorLen     = 20
	zipDirectoryHeaderSignature  = 0x02014b50
	zipDirectoryEndSignature     = 0x06054b50
	zipDirectory64EndSignature   = 0x06064b50
	zipDirectory64LocSignature   = 0x07064b50
	zipDirectoryEndShortWindow   = 1024
	zipDirectoryEndSearchWindow  = 65 * 1024
	zipDirectoryRecords64Marker  = 0xffff
	zipDirectorySizeOffsetMarker = 0xffffffff
)

type zipReader struct {
	raw    []byte
	reader *zip.Reader
	index  map[string]*zip.File
}

func openZip(raw []byte) (*zipReader, error) {
	if err := checkZipEntryCount(raw); err != nil {
		return nil, err
	}
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("opening zip: %w", err)
	}
	reader.RegisterDecompressor(zip.Deflate, newFlateReader)
	if err := checkArchiveEntryCount(len(reader.File)); err != nil {
		return nil, err
	}

	index := make(map[string]*zip.File, len(reader.File))
	for _, f := range reader.File {
		if _, seen := index[f.Name]; !seen {
			index[f.Name] = f
		}
	}

	return &zipReader{
		raw:    raw,
		reader: reader,
		index:  index,
	}, nil
}

func checkZipEntryCount(raw []byte) error {
	return checkZipEntryCountLimitContext(context.Background(), bytes.NewReader(raw), int64(len(raw)), maxArchiveEntries)
}

// checkZipEntryCountLimitContext counts central directory headers before
// archive/zip builds its file list. Only the limit is enforced here. Malformed
// directories are left for archive/zip to accept or reject.
func checkZipEntryCountLimitContext(ctx context.Context, input io.ReaderAt, size int64, limit int) error {
	start, end, ok, err := zipCentralDirectoryBounds(input, size)
	if err != nil || !ok {
		return err
	}

	headers := bufio.NewReader(io.NewSectionReader(input, start, end-start))
	var header [zipDirectoryHeaderLen]byte
	count := 0
	for remaining := end - start; remaining >= zipDirectoryHeaderLen; {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := io.ReadFull(headers, header[:]); err != nil {
			return zipSourceError(err)
		}
		if binary.LittleEndian.Uint32(header[:]) != zipDirectoryHeaderSignature {
			break
		}
		nameLen := int(binary.LittleEndian.Uint16(header[28:]))
		extraLen := int(binary.LittleEndian.Uint16(header[30:]))
		commentLen := int(binary.LittleEndian.Uint16(header[32:]))
		recordLen := int64(zipDirectoryHeaderLen + nameLen + extraLen + commentLen)
		if recordLen > remaining {
			return nil
		}

		count++
		if count > limit {
			return fmt.Errorf("%w: count %d exceeds %d", ErrEntryLimit, count, limit)
		}
		if _, err := headers.Discard(nameLen + extraLen + commentLen); err != nil {
			return zipSourceError(err)
		}
		remaining -= recordLen
	}

	return nil
}

// Search 1 KiB before 65 KiB, as archive/zip does; most archives have no comment.
func zipDirectoryEndRecord(input io.ReaderAt, size int64) ([]byte, int64, bool, error) {
	for _, window := range [...]int64{zipDirectoryEndShortWindow, zipDirectoryEndSearchWindow} {
		tailStart := max(size-window, 0)
		tail := make([]byte, size-tailStart)
		if err := readZipSource(input, tail, tailStart); err != nil {
			return nil, 0, false, err
		}
		for offset := len(tail) - zipDirectoryEndLen; offset >= 0; offset-- {
			if binary.LittleEndian.Uint32(tail[offset:]) != zipDirectoryEndSignature {
				continue
			}
			commentLen := int(binary.LittleEndian.Uint16(tail[offset+20:]))
			if offset+zipDirectoryEndLen+commentLen > len(tail) {
				continue
			}
			return tail[offset : offset+zipDirectoryEndLen], tailStart + int64(offset), true, nil
		}
		if tailStart == 0 {
			break
		}
	}
	return nil, 0, false, nil
}

func zipCentralDirectoryBounds(input io.ReaderAt, size int64) (int64, int64, bool, error) {
	record, recordOffset, found, err := zipDirectoryEndRecord(input, size)
	if err != nil || !found {
		return 0, 0, false, err
	}

	directoryEnd := recordOffset
	directoryRecords := binary.LittleEndian.Uint16(record[10:])
	directorySize := uint64(binary.LittleEndian.Uint32(record[12:]))
	directoryOffset := uint64(binary.LittleEndian.Uint32(record[16:]))
	if directoryRecords == zipDirectoryRecords64Marker ||
		directorySize == zipDirectorySizeOffsetMarker ||
		directoryOffset == zipDirectorySizeOffsetMarker {
		recordOffset64, located, err := zip64LocatorOffset(input, recordOffset)
		if err != nil {
			return 0, 0, false, err
		}
		// Without a locator, archive/zip keeps the classic values, so count with them too.
		if located {
			var ok bool
			directoryEnd, directorySize, directoryOffset, ok, err = zip64DirectoryRecord(input, size, recordOffset64)
			if err != nil || !ok {
				return 0, 0, false, err
			}
		}
	}

	if directorySize > uint64(directoryEnd) {
		return 0, 0, false, nil
	}
	start := directoryEnd - int64(directorySize)
	if directoryOffset < uint64(start) {
		// ZIPs can have an executable prefix whose length is absent from offsets.
		var signature [zipSignatureLen]byte
		if err := readZipSource(input, signature[:], int64(directoryOffset)); err != nil {
			return 0, 0, false, err
		}
		if binary.LittleEndian.Uint32(signature[:]) == zipDirectoryHeaderSignature {
			start = int64(directoryOffset)
		}
	}
	return start, directoryEnd, true, nil
}

// zip64LocatorOffset accepts the same locators as archive/zip: one disk, with
// the ZIP64 end record on disk 0.
func zip64LocatorOffset(input io.ReaderAt, endOffset int64) (uint64, bool, error) {
	if endOffset < zipDirectory64LocatorLen {
		return 0, false, nil
	}
	var locator [zipDirectory64LocatorLen]byte
	if err := readZipSource(input, locator[:], endOffset-zipDirectory64LocatorLen); err != nil {
		return 0, false, err
	}
	if binary.LittleEndian.Uint32(locator[:]) != zipDirectory64LocSignature ||
		binary.LittleEndian.Uint32(locator[4:]) != 0 ||
		binary.LittleEndian.Uint32(locator[16:]) != 1 {
		return 0, false, nil
	}
	return binary.LittleEndian.Uint64(locator[8:]), true, nil
}

func zip64DirectoryRecord(input io.ReaderAt, size int64, recordOffset uint64) (int64, uint64, uint64, bool, error) {
	if size < zipDirectory64EndLen || recordOffset > uint64(size-zipDirectory64EndLen) {
		return 0, 0, 0, false, nil
	}
	var record [zipDirectory64EndLen]byte
	if err := readZipSource(input, record[:], int64(recordOffset)); err != nil {
		return 0, 0, 0, false, err
	}
	if binary.LittleEndian.Uint32(record[:]) != zipDirectory64EndSignature {
		return 0, 0, 0, false, nil
	}
	directorySize := binary.LittleEndian.Uint64(record[40:])
	directoryOffset := binary.LittleEndian.Uint64(record[48:])
	return int64(recordOffset), directorySize, directoryOffset, true, nil
}

func readZipSource(input io.ReaderAt, p []byte, offset int64) error {
	n, err := input.ReadAt(p, offset)
	if err == io.EOF && n == len(p) {
		return nil
	}
	if err != nil {
		return zipSourceError(err)
	}
	return nil
}

// The size is trusted to bound every read, so EOF means the source is shorter
// than the caller declared.
func zipSourceError(err error) error {
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return fmt.Errorf("reading zip central directory: %w", err)
}

func (z *zipReader) List() ([]FileInfo, error) {
	files := make([]FileInfo, 0, len(z.reader.File))

	for _, f := range z.reader.File {
		files = append(files, fileInfoFromZip(f))
	}

	return files, nil
}

func (z *zipReader) ListDir(dirPath string) ([]FileInfo, error) {
	dirPath = normalizeDir(dirPath)
	var files []FileInfo

	// Track directories we've seen to avoid duplicates
	seenDirs := make(map[string]bool)

	for _, f := range z.reader.File {
		path := f.Name

		// Check if this file/dir is directly in the requested directory
		if isInDir(path, dirPath) {
			if f.FileInfo().IsDir() {
				name := strings.TrimSuffix(strings.TrimPrefix(path, dirPath), "/")
				if seenDirs[name] {
					continue
				}
				seenDirs[name] = true
			}
			files = append(files, fileInfoFromZip(f))
			continue
		}

		// Check if we should add a subdirectory entry
		if dirPath == "" || strings.HasPrefix(path, dirPath) {
			rel := strings.TrimSuffix(strings.TrimPrefix(path, dirPath), "/")
			if i := strings.IndexByte(rel, '/'); i >= 0 {
				name := rel[:i]
				if !seenDirs[name] {
					seenDirs[name] = true
					files = append(files, FileInfo{
						Path:  dirPath + name + "/",
						Name:  name,
						IsDir: true,
					})
				}
			}
		}
	}

	return files, nil
}

func (z *zipReader) Extract(filePath string) (io.ReadCloser, error) {
	f, ok := z.index[filePath]
	if !ok {
		return nil, fmt.Errorf("file not found: %s", filePath)
	}
	if f.FileInfo().IsDir() {
		return nil, fmt.Errorf("path is a directory: %s", filePath)
	}
	return f.Open()
}

func (z *zipReader) Hash(algo string) (string, error) {
	return hashRaw(z.raw, algo)
}

func (z *zipReader) Close() error {
	z.raw = nil
	z.reader = nil
	z.index = nil
	return nil
}

func fileInfoFromZip(f *zip.File) FileInfo {
	return FileInfo{
		Path:           f.Name,
		Name:           extractName(f.Name),
		Size:           int64(f.UncompressedSize64),
		CompressedSize: int64(f.CompressedSize64),
		ModTime:        f.Modified,
		IsDir:          f.FileInfo().IsDir(),
		Mode:           uint32(f.Mode()),
		HasMode:        zipHasUnixMode(&f.FileHeader),
	}
}

func zipHasUnixMode(h *zip.FileHeader) bool {
	switch h.CreatorVersion >> zipCreatorShift {
	case zipCreatorUnix, zipCreatorMacOSX:
		return h.ExternalAttrs>>zipUnixModeShift != 0
	default:
		return false
	}
}

// RegisterDecompressor bypasses archive/zip's internal flate reader pool,
// so pool klauspost readers here. archive/zip hands the decompressor an
// io.SectionReader with no ReadByte, which klauspost's Reset would wrap in
// a fresh bufio.Reader on every call; pooling one alongside the decoder and
// passing it to Reset avoids that and hits klauspost's *bufio.Reader fast
// path.
var flateReaderPool sync.Pool

type flateState struct {
	br *bufio.Reader
	fr io.ReadCloser
}

type pooledFlateReader struct {
	s *flateState
}

func newFlateReader(r io.Reader) io.ReadCloser {
	s, _ := flateReaderPool.Get().(*flateState)
	if s == nil {
		br := bufio.NewReader(r)
		return &pooledFlateReader{s: &flateState{br: br, fr: flate.NewReader(br)}}
	}
	s.br.Reset(r)
	_ = s.fr.(flate.Resetter).Reset(s.br, nil)
	return &pooledFlateReader{s: s}
}

func (p *pooledFlateReader) Read(b []byte) (int, error) {
	return p.s.fr.Read(b)
}

func (p *pooledFlateReader) Close() error {
	if p.s == nil {
		return nil
	}
	err := p.s.fr.Close()
	p.s.br.Reset(nil)
	flateReaderPool.Put(p.s)
	p.s = nil
	return err
}

func extractName(path string) string {
	path = strings.TrimSuffix(path, "/")
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[idx+1:]
	}
	return path
}
