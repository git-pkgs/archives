package archives

import (
	"archive/zip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// OpenStreamReaderAt opens ZIP or conda input without buffering the archive.
// size is the complete compressed input size. The caller owns content and must
// keep it open and unchanged until the stream is closed. Unread ZIP entries are
// skipped without decoding; their declared sizes still count against limits.
func OpenStreamReaderAt(filename string, content io.ReaderAt, size int64, options StreamOptions) (*Stream, error) {
	return OpenStreamReaderAtContext(context.Background(), filename, content, size, options)
}

// OpenStreamReaderAtContext is OpenStreamReaderAt with cancellation before and
// after each source read. Cancellation does not close or interrupt the input.
func OpenStreamReaderAtContext(ctx context.Context, filename string, content io.ReaderAt, size int64, options StreamOptions) (*Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	options, err := options.defaults()
	if err != nil {
		return nil, err
	}
	if size < 0 {
		return nil, fmt.Errorf("negative archive size: %d", size)
	}
	if size > options.MaxInputBytes {
		return nil, ErrInputLimit
	}
	input := contextReaderAt{ctx: ctx, reader: content}
	format, _, err := streamFormat(filename, io.NewSectionReader(input, 0, size))
	if err != nil {
		return nil, err
	}
	if format != formatZIP && format != formatConda {
		return nil, fmt.Errorf("%w: random-access archive format: %s", errors.ErrUnsupported, filename)
	}
	if err := checkZipReaderAt(input, size, options.MaxEntries); err != nil {
		return nil, err
	}
	reader, err := zip.NewReader(input, size)
	if err != nil {
		return nil, fmt.Errorf("opening zip: %w", err)
	}
	if len(reader.File) > options.MaxEntries {
		return nil, ErrEntryLimit
	}
	reader.RegisterDecompressor(zip.Deflate, newFlateReader)
	var source streamSource = &zipStream{ctx: ctx, reader: reader, skipUnread: true}
	if format == formatConda {
		source = &condaStream{outer: &Stream{ctx: ctx, source: source, options: containerOptions(options)}}
	}
	return &Stream{ctx: ctx, source: source, options: options}, nil
}

// OpenBrowserReaderAt prepares ZIP or conda input for repeated browsing without
// copying it. The caller owns content and must keep it open and unchanged until
// browsing and all extracted files finish. Limits apply to each pass. This API
// does not implement Reader or whole-artifact hashing; hash the source separately.
func OpenBrowserReaderAt(filename string, content io.ReaderAt, size int64, options BrowseOptions) (*Browser, error) {
	return OpenBrowserReaderAtContext(context.Background(), filename, content, size, options)
}

// OpenBrowserReaderAtContext is OpenBrowserReaderAt with cancellation across
// every pass, including reads from extracted files.
func OpenBrowserReaderAtContext(ctx context.Context, filename string, content io.ReaderAt, size int64, options BrowseOptions) (*Browser, error) {
	stream, err := OpenStreamReaderAtContext(ctx, filename, content, size, options.Limits)
	if err != nil {
		return nil, err
	}
	if err := stream.Close(); err != nil {
		return nil, err
	}
	return &Browser{ctx: ctx, filename: filename, options: options, random: content, size: size}, nil
}

// Check both the advertised count and the actual headers before archive/zip
// allocates its file slice. Read only the end records and central headers.
func checkZipReaderAt(input io.ReaderAt, size int64, limit int) error {
	tailSize := min(size, int64(zipDirectoryEndSearchWindow))
	tail := make([]byte, int(tailSize))
	if _, err := input.ReadAt(tail, size-tailSize); err != nil {
		return err
	}
	for offset := len(tail) - zipDirectoryEndLen; offset >= 0; offset-- {
		if binary.LittleEndian.Uint32(tail[offset:]) != zipDirectoryEndSignature {
			continue
		}
		end := tail[offset:]
		if offset+zipDirectoryEndLen+int(binary.LittleEndian.Uint16(end[20:])) > len(tail) {
			continue
		}
		directoryEnd := size - tailSize + int64(offset)
		count := uint64(binary.LittleEndian.Uint16(end[10:]))
		length := uint64(binary.LittleEndian.Uint32(end[12:]))
		start := uint64(binary.LittleEndian.Uint32(end[16:]))
		if count == zipDirectoryRecords64Marker || length == zipDirectorySizeOffsetMarker || start == zipDirectorySizeOffsetMarker {
			var locator [zipDirectory64LocatorLen]byte
			if directoryEnd < int64(len(locator)) {
				return zip.ErrFormat
			}
			if _, err := input.ReadAt(locator[:], directoryEnd-int64(len(locator))); err != nil {
				return err
			}
			if binary.LittleEndian.Uint32(locator[:]) != zipDirectory64LocSignature {
				return zip.ErrFormat
			}
			recordOffset := binary.LittleEndian.Uint64(locator[8:])
			var record [zipDirectory64EndLen]byte
			if size < int64(len(record)) || recordOffset > uint64(size-int64(len(record))) {
				return zip.ErrFormat
			}
			if _, err := input.ReadAt(record[:], int64(recordOffset)); err != nil {
				return err
			}
			if binary.LittleEndian.Uint32(record[:]) != zipDirectory64EndSignature {
				return zip.ErrFormat
			}
			directoryEnd = int64(recordOffset)
			count = binary.LittleEndian.Uint64(record[32:])
			length = binary.LittleEndian.Uint64(record[40:])
			start = binary.LittleEndian.Uint64(record[48:])
		}
		if count > uint64(limit) {
			return ErrEntryLimit
		}
		if length > uint64(directoryEnd) || start > uint64(directoryEnd)-length {
			return zip.ErrFormat
		}
		// ZIPs can have an executable prefix whose length is absent from offsets.
		actualStart := directoryEnd - int64(length)
		if start < uint64(actualStart) {
			var signature [4]byte
			if _, err := input.ReadAt(signature[:], int64(start)); err != nil {
				return err
			}
			if binary.LittleEndian.Uint32(signature[:]) == zipDirectoryHeaderSignature {
				actualStart = int64(start)
			}
		}
		actualCount := 0
		for pos := actualStart; pos < directoryEnd; {
			var header [zipDirectoryHeaderLen]byte
			if directoryEnd-pos < int64(len(header)) {
				return zip.ErrFormat
			}
			if _, err := input.ReadAt(header[:], pos); err != nil {
				return err
			}
			if binary.LittleEndian.Uint32(header[:]) != zipDirectoryHeaderSignature {
				return zip.ErrFormat
			}
			recordLen := int64(zipDirectoryHeaderLen) + int64(binary.LittleEndian.Uint16(header[28:])) + int64(binary.LittleEndian.Uint16(header[30:])) + int64(binary.LittleEndian.Uint16(header[32:]))
			if recordLen > directoryEnd-pos {
				return zip.ErrFormat
			}
			actualCount++
			if actualCount > limit {
				return ErrEntryLimit
			}
			pos += recordLen
		}
		if uint64(actualCount) != count {
			return zip.ErrFormat
		}
		return nil
	}
	return zip.ErrFormat
}
