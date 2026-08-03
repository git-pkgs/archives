package archives

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"strings"
)

// zipUnixModeShift is the bit offset of the Unix st_mode field within a
// ZIP entry's external attributes word. Unix permissions are only present
// when the creator system in CreatorVersion is Unix or macOS; other creators
// may store unrelated data in the high word, and archive/zip.FileHeader.Mode
// then returns a synthesised 0666/0444 that callers should not treat as a
// stored mode.
const (
	zipUnixModeShift = 16
	zipCreatorShift  = 8
	zipCreatorUnix   = 3
	zipCreatorMacOSX = 19
)

type zipReader struct {
	raw    []byte
	reader *zip.Reader
	index  map[string]*zip.File
}

func openZip(raw []byte) (*zipReader, error) {
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("opening zip: %w", err)
	}

	index := make(map[string]*zip.File, len(reader.File))
	for _, f := range reader.File {
		if _, seen := index[f.Name]; !seen {
			index[f.Name] = f
		}
	}

	return &zipReader{
		raw:    raw,
		reader: reader,
		index:  index,
	}, nil
}

func (z *zipReader) List() ([]FileInfo, error) {
	files := make([]FileInfo, 0, len(z.reader.File))

	for _, f := range z.reader.File {
		files = append(files, fileInfoFromZip(f))
	}

	return files, nil
}

func (z *zipReader) ListDir(dirPath string) ([]FileInfo, error) {
	dirPath = normalizeDir(dirPath)
	var files []FileInfo

	// Track directories we've seen to avoid duplicates
	seenDirs := make(map[string]bool)

	for _, f := range z.reader.File {
		path := f.Name

		// Check if this file/dir is directly in the requested directory
		if isInDir(path, dirPath) {
			if f.FileInfo().IsDir() {
				seenDirs[path] = true
			}
			files = append(files, fileInfoFromZip(f))
			continue
		}

		// Check if we should add a subdirectory entry
		if dirPath == "" || strings.HasPrefix(path, dirPath) {
			rel := strings.TrimPrefix(path, dirPath)
			parts := strings.Split(strings.TrimSuffix(rel, "/"), "/")
			if len(parts) > 1 {
				// This file is in a subdirectory
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

func (z *zipReader) Extract(filePath string) (io.ReadCloser, error) {
	f, ok := z.index[filePath]
	if !ok {
		return nil, fmt.Errorf("file not found: %s", filePath)
	}
	if f.FileInfo().IsDir() {
		return nil, fmt.Errorf("path is a directory: %s", filePath)
	}
	return f.Open()
}

func (z *zipReader) Hash(algo string) (string, error) {
	return hashRaw(z.raw, algo)
}

func (z *zipReader) Close() error {
	z.raw = nil
	z.reader = nil
	z.index = nil
	return nil
}

func fileInfoFromZip(f *zip.File) FileInfo {
	return FileInfo{
		Path:           f.Name,
		Name:           extractName(f.Name),
		Size:           int64(f.UncompressedSize64),
		CompressedSize: int64(f.CompressedSize64),
		ModTime:        f.Modified,
		IsDir:          f.FileInfo().IsDir(),
		Mode:           uint32(f.Mode()),
		HasMode:        zipHasUnixMode(&f.FileHeader),
	}
}

func zipHasUnixMode(h *zip.FileHeader) bool {
	switch h.CreatorVersion >> zipCreatorShift {
	case zipCreatorUnix, zipCreatorMacOSX:
		return h.ExternalAttrs>>zipUnixModeShift != 0
	default:
		return false
	}
}

func extractName(path string) string {
	path = strings.TrimSuffix(path, "/")
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[idx+1:]
	}
	return path
}
