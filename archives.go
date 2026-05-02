// Package archives provides in-memory archive reading and browsing capabilities.
//
// It supports multiple archive formats including:
//   - ZIP (.zip, .jar, .whl, .nupkg)
//   - TAR (.tar, .tar.gz, .tgz, .tar.bz2, .tar.xz)
//   - GEM (.gem - Ruby gems with nested tar structure)
//
// The package is designed to work entirely in memory without writing to disk,
// making it suitable for browsing cached artifacts on-demand.
package archives

import (
	"bytes"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
)

// FileInfo represents metadata about a file in an archive.
type FileInfo struct {
	Path           string    // Full path within archive
	Name           string    // Base name
	Size           int64     // Uncompressed size in bytes
	ModTime        time.Time // Modification time
	IsDir          bool      // Whether this is a directory
	Mode           uint32    // File mode/permissions
	CompressedSize int64     // Compressed size (if available)
}

// Reader provides methods to browse and extract files from archives.
type Reader interface {
	// List returns all files in the archive.
	List() ([]FileInfo, error)

	// ListDir returns files in a specific directory path.
	// Use "" or "/" for root directory.
	ListDir(dirPath string) ([]FileInfo, error)

	// Extract reads a specific file from the archive.
	// Returns io.ReadCloser for the file content.
	Extract(filePath string) (io.ReadCloser, error)

	// Hash returns the hex-encoded digest of the raw archive bytes using
	// the named algorithm. Supported algorithms are SHA256, SHA512, SHA1
	// and MD5. The hash is computed over the original archive as passed
	// to Open, not the decompressed contents.
	Hash(algo string) (string, error)

	// Close releases resources associated with the reader.
	Close() error
}

// Open creates an archive reader for the given content.
// The filename is used to detect the archive format.
// The content reader will be read entirely into memory.
//
//nolint:ireturn // factory function returning interface by design
func Open(filename string, content io.Reader) (Reader, error) {
	format := detectFormat(filename)
	if format == "" {
		return nil, fmt.Errorf("unsupported archive format: %s", filename)
	}

	raw, err := io.ReadAll(content)
	if err != nil {
		return nil, fmt.Errorf("reading archive content: %w", err)
	}

	var reader Reader

	switch format {
	case "zip":
		reader, err = openZip(raw)
	case "tar":
		reader, err = openTar(raw, "")
	case "tar.gz", "tgz":
		reader, err = openTar(raw, "gzip")
	case "tar.bz2":
		reader, err = openTar(raw, "bzip2")
	case "tar.xz":
		reader, err = openTar(raw, "xz")
	case "gem":
		reader, err = openGem(raw)
	default:
		return nil, fmt.Errorf("unsupported format: %s", format)
	}

	return reader, err
}

// OpenBytes is like Open but accepts the archive content as a byte slice.
// The slice is retained for the lifetime of the Reader to support Hash.
//
//nolint:ireturn // factory function returning interface by design
func OpenBytes(filename string, content []byte) (Reader, error) {
	return Open(filename, bytes.NewReader(content))
}

// OpenWithPrefix opens an archive and strips the given prefix from all paths.
// This is useful for npm packages which wrap content in a "package/" directory.
//
//nolint:ireturn // factory function returning interface by design
func OpenWithPrefix(filename string, content io.Reader, stripPrefix string) (Reader, error) {
	reader, err := Open(filename, content)
	if err != nil {
		return nil, err
	}

	if stripPrefix == "" {
		return reader, nil
	}

	return &prefixStripper{
		reader: reader,
		prefix: stripPrefix,
	}, nil
}

// detectFormat determines archive format from filename extension.
func detectFormat(filename string) string {
	filename = strings.ToLower(filename)

	// Check for compound extensions first
	if strings.HasSuffix(filename, ".tar.gz") {
		return "tar.gz"
	}
	if strings.HasSuffix(filename, ".tar.bz2") {
		return "tar.bz2"
	}
	if strings.HasSuffix(filename, ".tar.xz") {
		return "tar.xz"
	}

	// Check simple extensions
	ext := path.Ext(filename)
	switch ext {
	case ".zip", ".jar", ".whl", ".nupkg", ".egg":
		return "zip"
	case ".tar":
		return "tar"
	case ".tgz":
		return "tgz"
	case ".gem":
		return "gem"
	default:
		return ""
	}
}

// normalizeDir normalizes directory path for consistent comparison.
func normalizeDir(dirPath string) string {
	dirPath = strings.TrimSpace(dirPath)
	dirPath = strings.Trim(dirPath, "/")
	if dirPath == "" {
		return ""
	}
	return dirPath + "/"
}

// isInDir checks if filePath is directly in dirPath (not in subdirectories).
func isInDir(filePath, dirPath string) bool {
	dirPath = normalizeDir(dirPath)

	// Normalize file path by trimming trailing slash
	filePath = strings.TrimSuffix(filePath, "/")

	// Root directory
	if dirPath == "" {
		// File is in root if it has no slashes
		parts := strings.Split(filePath, "/")
		return len(parts) == 1
	}

	// Check if file starts with directory path
	if !strings.HasPrefix(filePath+"/", dirPath) {
		return false
	}

	// Get relative path
	rel := strings.TrimPrefix(filePath, strings.TrimSuffix(dirPath, "/"))
	rel = strings.TrimPrefix(rel, "/")

	// Should have no more slashes
	return !strings.Contains(rel, "/")
}
