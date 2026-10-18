package archives

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
)

type nestedTarReader struct {
	raw        []byte
	dataReader Reader
}

func openGem(raw []byte) (*nestedTarReader, error) {
	return openNestedTar(raw, "data.tar.gz", "gem")
}

func openNestedTar(raw []byte, member, kind string) (*nestedTarReader, error) {
	tr := tar.NewReader(bytes.NewReader(raw))
	entryCount := 0

	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s not found in %s", member, kind)
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s tar: %w", kind, err)
		}
		entryCount++
		if err := checkArchiveEntryCount(entryCount); err != nil {
			return nil, err
		}

		if header.Name == member {
			dataContent, err := io.ReadAll(io.LimitReader(tr, maxDecompressedSize+1))
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", member, err)
			}
			if int64(len(dataContent)) > maxDecompressedSize {
				return nil, fmt.Errorf("%w: %s exceeds %d bytes", ErrDecompressLimit, member, maxDecompressedSize)
			}

			dataReader, err := openTar(dataContent, compressionGzip)
			if err != nil {
				return nil, fmt.Errorf("opening %s: %w", member, err)
			}

			return &nestedTarReader{raw: raw, dataReader: dataReader}, nil
		}
	}
}

func (g *nestedTarReader) List() ([]FileInfo, error) {
	return g.dataReader.List()
}

func (g *nestedTarReader) ListDir(dirPath string) ([]FileInfo, error) {
	return g.dataReader.ListDir(dirPath)
}

func (g *nestedTarReader) Extract(filePath string) (io.ReadCloser, error) {
	return g.dataReader.Extract(filePath)
}

func (g *nestedTarReader) Hash(algo string) (string, error) {
	return hashRaw(g.raw, algo)
}

func (g *nestedTarReader) Close() error {
	g.raw = nil
	if g.dataReader != nil {
		return g.dataReader.Close()
	}
	return nil
}
