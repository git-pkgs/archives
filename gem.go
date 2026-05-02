package archives

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
)

// gemReader handles Ruby .gem files which have a nested structure:
// - gem file is a tar archive containing metadata.gz and data.tar.gz
// - data.tar.gz contains the actual source code
type gemReader struct {
	raw        []byte
	dataReader Reader
}

func openGem(raw []byte) (*gemReader, error) {
	tr := tar.NewReader(bytes.NewReader(raw))

	// Find data.tar.gz in the gem
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("data.tar.gz not found in gem")
		}
		if err != nil {
			return nil, fmt.Errorf("reading gem tar: %w", err)
		}

		// Look for data.tar.gz
		if header.Name == "data.tar.gz" {
			dataContent, err := io.ReadAll(io.LimitReader(tr, maxDecompressedSize+1))
			if err != nil {
				return nil, fmt.Errorf("reading data.tar.gz: %w", err)
			}
			if int64(len(dataContent)) > maxDecompressedSize {
				return nil, fmt.Errorf("%w: data.tar.gz exceeds %d bytes", ErrDecompressLimit, maxDecompressedSize)
			}

			dataReader, err := openTar(dataContent, "gzip")
			if err != nil {
				return nil, fmt.Errorf("opening data.tar.gz: %w", err)
			}

			return &gemReader{raw: raw, dataReader: dataReader}, nil
		}
	}
}

func (g *gemReader) List() ([]FileInfo, error) {
	return g.dataReader.List()
}

func (g *gemReader) ListDir(dirPath string) ([]FileInfo, error) {
	return g.dataReader.ListDir(dirPath)
}

func (g *gemReader) Extract(filePath string) (io.ReadCloser, error) {
	return g.dataReader.Extract(filePath)
}

func (g *gemReader) Hash(algo string) (string, error) {
	return hashRaw(g.raw, algo)
}

func (g *gemReader) Close() error {
	g.raw = nil
	if g.dataReader != nil {
		return g.dataReader.Close()
	}
	return nil
}
