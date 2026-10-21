package archives

import (
	"bufio"
	"bytes"
	"context"
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
	ctx         context.Context
	source      streamSource
	options     StreamOptions
	expanded    int64
	entries     int
	err         error
	closed      bool
	unknownSize bool
	entryBytes  int64
}

type streamSource interface {
	io.ReadCloser
	Next() (*StreamEntry, error)
}

// OpenStream reads supported archives sequentially. ZIP and conda input is
// buffered up to MaxInputBytes for random access. Hex detection buffers an outer
// TAR prefix; TAR, gem, and Hex entry bodies are streamed. Compression detected
// by content exposes a non-TAR payload as one member named "content", with size -1.
// The caller owns content and must close it when needed.
func OpenStream(filename string, content io.Reader, options StreamOptions) (*Stream, error) {
	return OpenStreamContext(context.Background(), filename, content, options)
}

// OpenStreamContext is OpenStream with cancellation during reading and decoding.
// Cancellation is terminal. The caller owns content; canceling does not close it.
func OpenStreamContext(ctx context.Context, filename string, content io.Reader, options StreamOptions) (*Stream, error) {
	options, err := options.defaults()
	if err != nil {
		return nil, err
	}
	return openStream(ctx, filename, &streamInput{reader: content, remaining: options.MaxInputBytes}, nil, options)
}

// OpenStreamBytes reuses content without copying it, including ZIP and conda
// input. The caller must not modify the slice until the stream is closed.
func OpenStreamBytes(filename string, content []byte, options StreamOptions) (*Stream, error) {
	return OpenStreamBytesContext(context.Background(), filename, content, options)
}

// OpenStreamBytesContext is OpenStreamBytes with cancellation, including while
// decoding or skipping buffered ZIP and conda entries.
func OpenStreamBytesContext(ctx context.Context, filename string, content []byte, options StreamOptions) (*Stream, error) {
	options, err := options.defaults()
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > options.MaxInputBytes {
		return nil, ErrInputLimit
	}
	return openStream(ctx, filename, bytes.NewReader(content), content, options)
}

func openStream(ctx context.Context, filename string, content io.Reader, raw []byte, options StreamOptions) (*Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	content = contextReader{ctx: ctx, reader: content}
	format, content, err := streamFormat(filename, content)
	if err != nil {
		return nil, err
	}
	var source streamSource
	switch format {
	case formatZIP, formatConda:
		if raw == nil {
			raw, err = io.ReadAll(content)
			if err != nil {
				return nil, err
			}
		}
		source, err = newZipStream(ctx, raw, options, format == formatConda)
	case formatGem:
		source = &nestedTarStream{outer: newContainerTar(ctx, content, options), member: "data.tar.gz", kind: "gem"}
	case formatTAR:
		source = &plainTarStream{ctx: ctx, content: content, options: options}
	case compressionGzip, compressionBzip2, compressionXZ, compressionZstd:
		source, err = newCompressedStream(ctx, content, format)
	default:
		compression, ok := streamCompression(format)
		if !ok {
			return nil, fmt.Errorf("%w: archive format: %s", errors.ErrUnsupported, filename)
		}
		source, err = newTarStream(ctx, content, compression)
	}
	if err != nil {
		if canceled := ctx.Err(); canceled != nil {
			return nil, canceled
		}
		return nil, err
	}
	return &Stream{ctx: ctx, source: source, options: options}, nil
}

func streamFormat(filename string, content io.Reader) (string, io.Reader, error) {
	format := detectFormat(filename)
	if format == "" {
		buffered := bufio.NewReaderSize(content, contentSniffSize)
		prefix, err := buffered.Peek(contentSniffSize)
		if err != nil && err != io.EOF {
			return "", nil, fmt.Errorf("reading archive content: %w", err)
		}
		format = detectContentPrefixFormat(prefix)
		content = buffered
	}
	if format == "" {
		return "", nil, fmt.Errorf("%w: archive format: %s", errors.ErrUnsupported, filename)
	}
	return format, content, nil
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
// the body is exposed. Unknown sizes count as bytes are read or skipped.
// MaxEntries counts visible entries; nested containers
// have a separate MaxEntries budget. Hidden TAR extension headers are excluded.
// Any iteration error is terminal.
func (s *Stream) Next() (*StreamEntry, error) {
	if s.closed {
		return nil, fs.ErrClosed
	}
	if s.err != nil {
		return nil, s.err
	}
	if err := s.ctx.Err(); err != nil {
		s.err = err
		return nil, err
	}
	if s.unknownSize {
		if _, err := io.Copy(io.Discard, s); err != nil {
			return nil, err
		}
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
	s.unknownSize = entry.Size == -1
	s.entryBytes = 0
	if !s.unknownSize {
		s.expanded += entry.Size
	}
	return entry, nil
}

func (s *Stream) checkEntry(entry *StreamEntry) error {
	if s.entries >= s.options.MaxEntries {
		return fmt.Errorf("%w: exceeds %d", ErrEntryLimit, s.options.MaxEntries)
	}
	if entry.Size < -1 || entry.Size > s.options.MaxEntryBytes {
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
	if err := s.ctx.Err(); err != nil {
		s.err = err
		return 0, err
	}
	var n int
	var err error
	if s.unknownSize {
		n, err = s.readUnknownSize(p)
	} else {
		n, err = s.source.Read(p)
	}
	if err != nil && err != io.EOF {
		s.err = err
	}
	return n, err
}

func (s *Stream) readUnknownSize(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	remaining := min(s.options.MaxEntryBytes-s.entryBytes, s.options.MaxExpandedBytes-s.expanded)
	if remaining == 0 {
		var probe [1]byte
		n, err := s.source.Read(probe[:])
		if n > 0 {
			if s.entryBytes == s.options.MaxEntryBytes {
				return 0, ErrEntrySizeLimit
			}
			return 0, ErrDecompressLimit
		}
		return 0, err
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := s.source.Read(p)
	s.entryBytes += int64(n)
	s.expanded += int64(n)
	return n, err
}

// Recover abandons a failed ZIP entry so the next call to Next can continue.
// TAR-based formats, including conda, return errors.ErrUnsupported, as does a
// stream with no pending error. Cancellation and resource-limit errors remain
// terminal. Recovery does not validate the discarded body.
func (s *Stream) Recover() error {
	if s.closed {
		return fs.ErrClosed
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if s.err == nil || errors.Is(s.err, io.EOF) {
		return errors.ErrUnsupported
	}
	if errors.Is(s.err, ErrInputLimit) || errors.Is(s.err, ErrEntryLimit) || errors.Is(s.err, ErrEntrySizeLimit) || errors.Is(s.err, ErrDecompressLimit) {
		return s.err
	}
	zip, ok := s.source.(*zipStream)
	if !ok {
		return errors.ErrUnsupported
	}
	zip.recover()
	s.err = nil
	return nil
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
