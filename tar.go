package archives

import (
	"archive/tar"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ulikunitz/xz"
)

var maxDecompressedSize int64 = 512 << 20 // 512 MiB

var ErrDecompressLimit = errors.New("decompressed content exceeds size limit")

type tarReader struct {
	raw   []byte
	files []tarFileEntry
}

type tarFileEntry struct {
	info FileInfo
	data []byte
}

func openTar(raw []byte, compression string) (*tarReader, error) {
	content := bytes.NewReader(raw)
	r := io.Reader(content)

	switch compression {
	case "gzip":
		gz, err := gzip.NewReader(content)
		if err != nil {
			return nil, fmt.Errorf("opening gzip: %w", err)
		}
		defer func() { _ = gz.Close() }()
		r = gz
	case "bzip2":
		r = bzip2.NewReader(content)
	case "xz":
		xzReader, err := xz.NewReader(content)
		if err != nil {
			return nil, fmt.Errorf("opening xz: %w", err)
		}
		r = xzReader
	}

	tr := tar.NewReader(r)
	var files []tarFileEntry
	var totalSize int64

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading tar: %w", err)
		}

		info := FileInfo{
			Path:    header.Name,
			Name:    extractName(header.Name),
			Size:    header.Size,
			ModTime: header.ModTime,
			IsDir:   header.Typeflag == tar.TypeDir,
			Mode:    uint32(header.Mode),
		}

		var data []byte
		if !info.IsDir {
			remaining := maxDecompressedSize - totalSize
			data, err = io.ReadAll(io.LimitReader(tr, remaining+1))
			if err != nil {
				return nil, fmt.Errorf("reading file %s: %w", header.Name, err)
			}
			if int64(len(data)) > remaining {
				return nil, fmt.Errorf("%w: exceeds %d bytes", ErrDecompressLimit, maxDecompressedSize)
			}
			totalSize += int64(len(data))
		}

		files = append(files, tarFileEntry{
			info: info,
			data: data,
		})
	}

	return &tarReader{raw: raw, files: files}, nil
}

func (t *tarReader) List() ([]FileInfo, error) {
	files := make([]FileInfo, len(t.files))
	for i, f := range t.files {
		files[i] = f.info
	}
	return files, nil
}

func (t *tarReader) ListDir(dirPath string) ([]FileInfo, error) {
	dirPath = normalizeDir(dirPath)
	var files []FileInfo
	seenDirs := make(map[string]bool)

	for _, f := range t.files {
		path := f.info.Path

		// Check if this file/dir is directly in the requested directory
		if isInDir(path, dirPath) {
			if f.info.IsDir {
				seenDirs[path] = true
			}
			files = append(files, f.info)
			continue
		}

		// Check if we should add a subdirectory entry
		if dirPath == "" || strings.HasPrefix(path, dirPath) {
			rel := strings.TrimPrefix(path, dirPath)
			parts := strings.Split(strings.TrimSuffix(rel, "/"), "/")
			if len(parts) > 1 {
				subdir := dirPath + parts[0] + "/"
				if !seenDirs[subdir] {
					seenDirs[subdir] = true
					files = append(files, FileInfo{
						Path:  subdir,
						Name:  parts[0],
						IsDir: true,
					})
				}
			}
		}
	}

	return files, nil
}

func (t *tarReader) Extract(filePath string) (io.ReadCloser, error) {
	for _, f := range t.files {
		if f.info.Path == filePath {
			if f.info.IsDir {
				return nil, fmt.Errorf("path is a directory: %s", filePath)
			}
			return io.NopCloser(bytes.NewReader(f.data)), nil
		}
	}
	return nil, fmt.Errorf("file not found: %s", filePath)
}

func (t *tarReader) Hash(algo string) (string, error) {
	return hashRaw(t.raw, algo)
}

func (t *tarReader) Close() error {
	t.raw = nil
	t.files = nil
	return nil
}
