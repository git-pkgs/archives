package archives

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"io"

	"github.com/git-pkgs/magic"
)

const compressedMemberName = "content"

type compressedStream struct {
	reader  io.Reader
	closer  io.Closer
	started bool
}

//nolint:ireturn // decompressed content may contain TAR entries or one file
func newCompressedStream(ctx context.Context, content io.Reader, compression string) (streamSource, error) {
	reader, closer, err := tarContentReader(contextReader{ctx: ctx, reader: content}, compression)
	if err != nil {
		return nil, err
	}
	buffered := bufio.NewReaderSize(contextReader{ctx: ctx, reader: reader}, contentSniffSize)
	prefix, err := buffered.Peek(contentSniffSize)
	if err != nil && err != io.EOF {
		if closer != nil {
			_ = closer.Close()
		}
		return nil, err
	}
	// The TAR parser also accepts v7 headers without a ustar signature.
	_, headerErr := tar.NewReader(bytes.NewReader(prefix)).Next()
	if magic.DetectPrefix(prefix).Format == magic.FormatTAR || headerErr == nil {
		return &tarStream{reader: tar.NewReader(buffered), closer: closer}, nil
	}
	return &compressedStream{reader: buffered, closer: closer}, nil
}

func (s *compressedStream) Next() (*StreamEntry, error) {
	if s.started {
		return nil, io.EOF
	}
	s.started = true
	return &StreamEntry{FileInfo: FileInfo{Path: compressedMemberName, Name: compressedMemberName, Size: -1}}, nil
}

func (s *compressedStream) Read(p []byte) (int, error) {
	if !s.started {
		return 0, io.EOF
	}
	return s.reader.Read(p)
}

func (s *compressedStream) Close() error {
	if s.closer != nil {
		return s.closer.Close()
	}
	return nil
}

func openCompressed(raw []byte) (*tarReader, error) {
	stream, err := OpenStreamBytes("", raw, StreamOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()
	result := &tarReader{raw: raw, index: make(map[string]int)}
	for {
		entry, err := stream.Next()
		if err == io.EOF {
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(stream)
		if err != nil {
			return nil, err
		}
		info := entry.FileInfo
		if info.Size < 0 {
			info.Size = int64(len(data))
		}
		if _, exists := result.index[info.Path]; !exists {
			result.index[info.Path] = len(result.files)
		}
		result.files = append(result.files, tarFileEntry{info: info, data: data})
	}
}
