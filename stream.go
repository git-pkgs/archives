package archives

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
)

var ErrEntrySizeLimit = errors.New("archive entry exceeds size limit")
var ErrInputLimit = errors.New("archive input exceeds size limit")

// StreamOptions limits entry bodies, including skipped entries. Zero values
// use 512 MiB for byte limits and 100,000 entries; negative values are invalid.
// Decoder buffers and archive metadata require additional memory.
type StreamOptions struct {
	MaxInputBytes    int64
	MaxEntryBytes    int64
	MaxExpandedBytes int64
	MaxEntries       int
}

// StreamEntry includes link metadata needed to hash TAR links. ZIP symlink
// targets are stored in the entry body. Hard links refer to another TAR path.
type StreamEntry struct {
	FileInfo
	Linkname   string
	IsHardlink bool
}

// Stream reads entries in archive order without retaining expanded bodies.
// Paths and duplicate entries are preserved. A Stream is not safe for
// concurrent use. It does not implement the random-access Reader interface.
type Stream struct {
	source   streamSource
	options  StreamOptions
	expanded int64
	entries  int
	err      error
	closed   bool
}

type streamSource interface {
	io.ReadCloser
	Next() (*StreamEntry, error)
}

// OpenStream reads supported archives sequentially. ZIP and conda input is
// buffered up to MaxInputBytes for random access; TAR and gem input is streamed.
// The caller owns content and must close it when needed.
func OpenStream(filename string, content io.Reader, options StreamOptions) (*Stream, error) {
	options, err := options.defaults()
	if err != nil {
		return nil, err
	}
	return openStream(filename, &streamInput{reader: content, remaining: options.MaxInputBytes}, nil, options)
}

// OpenStreamBytes reuses content without copying it, including ZIP and conda
// input. The caller must not modify the slice until the stream is closed.
func OpenStreamBytes(filename string, content []byte, options StreamOptions) (*Stream, error) {
	options, err := options.defaults()
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > options.MaxInputBytes {
		return nil, ErrInputLimit
	}
	return openStream(filename, bytes.NewReader(content), content, options)
}

func openStream(filename string, content io.Reader, raw []byte, options StreamOptions) (*Stream, error) {
	format := detectFormat(filename)
	if format == "" {
		buffered := bufio.NewReaderSize(content, contentSniffSize)
		prefix, err := buffered.Peek(contentSniffSize)
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("reading archive content: %w", err)
		}
		format = detectContentPrefixFormat(prefix)
		content = buffered
	}
	var source streamSource
	var err error
	switch format {
	case formatZIP, formatConda:
		if raw == nil {
			raw, err = io.ReadAll(content)
			if err != nil {
				return nil, err
			}
		}
		source, err = newZipStream(raw, options, format == formatConda)
	case formatGem:
		source = &gemStream{outer: newContainerTar(content, options)}
	default:
		compression, ok := streamCompression(format)
		if !ok {
			return nil, fmt.Errorf("%w: archive format: %s", errors.ErrUnsupported, filename)
		}
		source, err = newTarStream(content, compression)
	}
	if err != nil {
		return nil, err
	}
	return &Stream{source: source, options: options}, nil
}

func (o StreamOptions) defaults() (StreamOptions, error) {
	if o.MaxInputBytes < 0 || o.MaxEntryBytes < 0 || o.MaxExpandedBytes < 0 || o.MaxEntries < 0 {
		return o, errors.New("stream limits must not be negative")
	}
	if o.MaxInputBytes == 0 {
		o.MaxInputBytes = maxDecompressedSize
	}
	if o.MaxEntryBytes == 0 {
		o.MaxEntryBytes = maxDecompressedSize
	}
	if o.MaxExpandedBytes == 0 {
		o.MaxExpandedBytes = maxDecompressedSize
	}
	if o.MaxEntries == 0 {
		o.MaxEntries = maxArchiveEntries
	}
	return o, nil
}

func streamCompression(format string) (string, bool) {
	switch format {
	case formatTAR:
		return "", true
	case formatTarGzip, formatTGZ:
		return compressionGzip, true
	case formatTarBzip2:
		return compressionBzip2, true
	case formatTarXZ:
		return compressionXZ, true
	case formatTarZstd:
		return compressionZstd, true
	default:
		return "", false
	}
}

// Next discards the unread body and returns the next entry, or io.EOF.
// Header sizes, including sparse logical sizes, count against limits before
// the body is exposed. MaxEntries counts visible entries; nested containers
// have a separate MaxEntries budget. Hidden TAR extension headers are excluded.
// Any iteration error is terminal.
func (s *Stream) Next() (*StreamEntry, error) {
	if s.closed {
		return nil, fs.ErrClosed
	}
	if s.err != nil {
		return nil, s.err
	}
	entry, err := s.source.Next()
	if err == nil {
		err = s.checkEntry(entry)
	}
	if err != nil {
		s.err = err
		return nil, err
	}
	s.entries++
	s.expanded += entry.Size
	return entry, nil
}

func (s *Stream) checkEntry(entry *StreamEntry) error {
	if s.entries >= s.options.MaxEntries {
		return fmt.Errorf("%w: exceeds %d", ErrEntryLimit, s.options.MaxEntries)
	}
	if entry.Size < 0 || entry.Size > s.options.MaxEntryBytes {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrEntrySizeLimit, entry.Path, s.options.MaxEntryBytes)
	}
	if entry.Size > s.options.MaxExpandedBytes-s.expanded {
		return fmt.Errorf("%w: exceeds %d bytes", ErrDecompressLimit, s.options.MaxExpandedBytes)
	}
	return nil
}

// Read reads the current entry and returns io.EOF at its end or before Next.
// Non-EOF errors prevent further iteration.
func (s *Stream) Read(p []byte) (int, error) {
	if s.closed {
		return 0, fs.ErrClosed
	}
	if s.err != nil {
		return 0, s.err
	}
	n, err := s.source.Read(p)
	if err != nil && err != io.EOF {
		s.err = err
	}
	return n, err
}

// Close releases resources without draining or closing the input. Unread
// content and compressed trailers after a TAR end marker are not validated.
func (s *Stream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	err := s.source.Close()
	s.source = nil
	return err
}

type streamInput struct {
	reader    io.Reader
	remaining int64
}

func (r *streamInput) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.remaining == 0 {
		var probe [1]byte
		n, err := r.reader.Read(probe[:])
		if n != 0 {
			return 0, ErrInputLimit
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	return n, err
}
