package archives

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"strings"
)

type zipStream struct {
	reader  *zip.Reader
	index   int
	file    *zip.File
	current io.ReadCloser
}

//nolint:ireturn // archive formats share a sequential source
func newZipStream(raw []byte, options StreamOptions, conda bool) (streamSource, error) {
	if err := checkZipEntryCountLimit(raw, options.MaxEntries); err != nil {
		return nil, err
	}
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("opening zip: %w", err)
	}
	if len(reader.File) > options.MaxEntries {
		return nil, ErrEntryLimit
	}
	reader.RegisterDecompressor(zip.Deflate, newFlateReader)
	z := &zipStream{reader: reader}
	if conda {
		return &condaStream{outer: &Stream{source: z, options: containerOptions(options)}}, nil
	}
	return z, nil
}

func (z *zipStream) Next() (*StreamEntry, error) {
	if z.file != nil {
		if _, err := io.Copy(io.Discard, z); err != nil {
			return nil, err
		}
		if err := z.current.Close(); err != nil {
			return nil, err
		}
		z.current = nil
		z.file = nil
	}
	if z.index == len(z.reader.File) {
		return nil, io.EOF
	}
	z.file = z.reader.File[z.index]
	z.index++
	return &StreamEntry{FileInfo: fileInfoFromZip(z.file)}, nil
}

func (z *zipStream) Read(p []byte) (int, error) {
	if z.file == nil {
		return 0, io.EOF
	}
	if z.current == nil {
		var err error
		z.current, err = z.file.Open()
		if err != nil {
			return 0, err
		}
	}
	return z.current.Read(p)
}

func (z *zipStream) Close() error {
	z.reader = nil
	z.file = nil
	if z.current != nil {
		return z.current.Close()
	}
	return nil
}

type condaStream struct {
	outer *Stream
	inner *tarStream
	found bool
}

func (c *condaStream) Next() (*StreamEntry, error) {
	for {
		if c.inner != nil {
			entry, err := c.inner.Next()
			if err != io.EOF {
				return entry, err
			}
			if err := c.inner.Close(); err != nil {
				return nil, err
			}
			c.inner = nil
		}
		entry, err := c.outer.Next()
		if err == io.EOF && !c.found {
			return nil, fmt.Errorf("no pkg-*.tar.zst or info-*.tar.zst member in conda package")
		}
		if err != nil {
			return nil, err
		}
		if !strings.HasSuffix(entry.Path, ".tar.zst") ||
			(!strings.HasPrefix(entry.Path, "pkg-") && !strings.HasPrefix(entry.Path, "info-")) {
			continue
		}
		c.found = true
		c.inner, err = newTarStream(c.outer, compressionZstd)
		if err != nil {
			return nil, fmt.Errorf("opening %s: %w", entry.Path, err)
		}
	}
}

func (c *condaStream) Read(p []byte) (int, error) {
	if c.inner == nil {
		return 0, io.EOF
	}
	return c.inner.Read(p)
}

func (c *condaStream) Close() error {
	var err error
	if c.inner != nil {
		err = c.inner.Close()
	}
	outerErr := c.outer.Close()
	if err != nil {
		return err
	}
	return outerErr
}
