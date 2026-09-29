package archives

import (
	"archive/tar"
	"bytes"
	"compress/bzip2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

var (
	maxDecompressedSize int64 = 512 << 20 // 512 MiB
	maxArchiveEntries         = 100_000
)

var ErrDecompressLimit = errors.New("decompressed content exceeds size limit")
var ErrEntryLimit = errors.New("archive entry count exceeds limit")

const maxInitialEntryBuffer = 64 << 10

type tarReader struct {
	raw   []byte
	files []tarFileEntry
	index map[string]int
}

type tarFileEntry struct {
	info FileInfo
	data []byte
}

func openTar(raw []byte, compression string) (*tarReader, error) {
	return openTarWithInitialEntryCount(raw, compression, 0)
}

func openTarWithInitialEntryCount(raw []byte, compression string, initialEntryCount int) (*tarReader, error) {
	r, closer, err := tarContentReader(bytes.NewReader(raw), compression)
	if err != nil {
		return nil, err
	}
	if closer != nil {
		defer func() { _ = closer.Close() }()
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
		if err := checkArchiveEntryCount(initialEntryCount + len(files) + 1); err != nil {
			return nil, err
		}

		info := fileInfoFromTar(header)

		var data []byte
		if !info.IsDir {
			remaining := maxDecompressedSize - totalSize
			data, err = readTarEntry(tr, header.Size, remaining)
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

	index := make(map[string]int, len(files))
	for i, f := range files {
		if _, seen := index[f.info.Path]; !seen {
			index[f.info.Path] = i
		}
	}

	return &tarReader{raw: raw, files: files, index: index}, nil
}

func fileInfoFromTar(header *tar.Header) FileInfo {
	mode := header.FileInfo().Mode()
	// Hard links have no body, despite FileInfo reporting a regular file.
	if header.Typeflag == tar.TypeLink {
		mode |= fs.ModeIrregular
	}
	return FileInfo{
		Path: header.Name, Name: extractName(header.Name), Size: header.Size,
		ModTime: header.ModTime, IsDir: header.Typeflag == tar.TypeDir,
		Mode: uint32(mode), HasMode: true,
	}
}

func tarContentReader(content io.Reader, compression string) (io.Reader, io.Closer, error) {
	switch compression {
	case compressionGzip:
		gz, err := gzip.NewReader(content)
		if err != nil {
			return nil, nil, fmt.Errorf("opening gzip: %w", err)
		}
		return gz, gz, nil
	case compressionBzip2:
		return bzip2.NewReader(content), nil, nil
	case compressionXZ:
		r, err := xz.NewReader(content)
		if err != nil {
			return nil, nil, fmt.Errorf("opening xz: %w", err)
		}
		return r, nil, nil
	case compressionZstd:
		dec, err := zstd.NewReader(content, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, nil, fmt.Errorf("opening zstd: %w", err)
		}
		return dec, dec.IOReadCloser(), nil
	default:
		return content, nil, nil
	}
}

func readTarEntry(r io.Reader, size, remaining int64) ([]byte, error) {
	limited := io.LimitReader(r, remaining+1)
	if size <= bytes.MinRead || size > maxInitialEntryBuffer {
		return io.ReadAll(limited)
	}
	var buf bytes.Buffer
	// Bound the hint from untrusted headers and leave room for the EOF read.
	buf.Grow(int(min(size, remaining)) + bytes.MinRead)
	_, err := buf.ReadFrom(limited)
	return buf.Bytes(), err
}

func checkArchiveEntryCount(count int) error {
	if count > maxArchiveEntries {
		return fmt.Errorf("%w: count %d exceeds %d", ErrEntryLimit, count, maxArchiveEntries)
	}
	return nil
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
				name := strings.TrimSuffix(strings.TrimPrefix(path, dirPath), "/")
				if seenDirs[name] {
					continue
				}
				seenDirs[name] = true
			}
			files = append(files, f.info)
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

func (t *tarReader) Extract(filePath string) (io.ReadCloser, error) {
	i, ok := t.index[filePath]
	if !ok {
		return nil, fmt.Errorf("file not found: %s", filePath)
	}
	f := t.files[i]
	if f.info.IsDir {
		return nil, fmt.Errorf("path is a directory: %s", filePath)
	}
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

func (t *tarReader) Hash(algo string) (string, error) {
	return hashRaw(t.raw, algo)
}

func (t *tarReader) Close() error {
	t.raw = nil
	t.files = nil
	t.index = nil
	return nil
}
