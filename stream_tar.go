package archives

import (
	"archive/tar"
	"fmt"
	"io"
)

type tarStream struct {
	reader *tar.Reader
	closer io.Closer
}

func newTarStream(content io.Reader, compression string) (*tarStream, error) {
	r, closer, err := tarContentReader(content, compression)
	if err != nil {
		return nil, err
	}
	return &tarStream{reader: tar.NewReader(r), closer: closer}, nil
}

func (t *tarStream) Next() (*StreamEntry, error) {
	h, err := t.reader.Next()
	if err != nil {
		return nil, err
	}
	return &StreamEntry{
		FileInfo: fileInfoFromTar(h), Linkname: h.Linkname,
		IsHardlink: h.Typeflag == tar.TypeLink,
	}, nil
}

func (t *tarStream) Read(p []byte) (int, error) {
	return t.reader.Read(p)
}

func (t *tarStream) Close() error {
	t.reader = nil
	if t.closer != nil {
		return t.closer.Close()
	}
	return nil
}

func containerOptions(options StreamOptions) StreamOptions {
	options.MaxEntryBytes = options.MaxInputBytes
	options.MaxExpandedBytes = options.MaxInputBytes
	return options
}

func newContainerTar(content io.Reader, options StreamOptions) *Stream {
	return &Stream{
		source:  &tarStream{reader: tar.NewReader(content)},
		options: containerOptions(options),
	}
}

type gemStream struct {
	outer *Stream
	inner *tarStream
}

func (g *gemStream) Next() (*StreamEntry, error) {
	if g.inner == nil {
		for {
			entry, err := g.outer.Next()
			if err == io.EOF {
				return nil, fmt.Errorf("data.tar.gz not found in gem")
			}
			if err != nil {
				return nil, err
			}
			if entry.Path != "data.tar.gz" {
				continue
			}
			g.inner, err = newTarStream(g.outer, compressionGzip)
			if err != nil {
				return nil, fmt.Errorf("opening data.tar.gz: %w", err)
			}
			break
		}
	}
	return g.inner.Next()
}

func (g *gemStream) Read(p []byte) (int, error) {
	if g.inner == nil {
		return 0, io.EOF
	}
	return g.inner.Read(p)
}

func (g *gemStream) Close() error {
	var err error
	if g.inner != nil {
		err = g.inner.Close()
	}
	outerErr := g.outer.Close()
	if err != nil {
		return err
	}
	return outerErr
}
