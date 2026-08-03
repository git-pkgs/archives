// Package archives provides in-memory archive reading and browsing capabilities.
//
// It supports multiple archive formats including:
//   - ZIP (.zip, .jar, .whl, .nupkg, .egg, .vsix)
//   - TAR (.tar, .tar.gz, .tgz, .crate, .tar.bz2, .tar.xz)
//   - GEM (.gem - Ruby gems with nested tar structure)
//
// The .apk extension is routed by content since Android packages are ZIP
// and Alpine packages are gzipped tar. Filenames without a recognised
// extension are opened by inspecting the first bytes.
//
// The package is designed to work entirely in memory without writing to disk,
// making it suitable for browsing cached artifacts on-demand.
package archives

import (
	"bufio"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/git-pkgs/magic"
)

const (
	formatZIP        = "zip"
	formatTAR        = "tar"
	formatTarGzip    = "tar.gz"
	formatTGZ        = "tgz"
	formatTarBzip2   = "tar.bz2"
	formatTarXZ      = "tar.xz"
	formatGem        = "gem"
	contentSniffSize = 512
)

// FileInfo represents metadata about a file in an archive.
type FileInfo struct {
	Path           string    // Full path within archive
	Name           string    // Base name
	Size           int64     // Uncompressed size in bytes
	ModTime        time.Time // Modification time
	IsDir          bool      // Whether this is a directory
	Mode           uint32    // fs.FileMode value: permission bits plus fs.ModeType bits
	HasMode        bool      // Whether Mode was recorded by the archive
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
// The filename is used first to detect the archive format. If it has no
// supported extension, the content is checked for a supported physical format.
// Recognised archives are read entirely into memory. An unrecognised stream
// with no supported extension is rejected after reading at most 512 bytes.
//
//nolint:ireturn // factory function returning interface by design
func Open(filename string, content io.Reader) (Reader, error) {
	format := detectFormat(filename)
	if format == "" {
		buffered := bufio.NewReaderSize(content, contentSniffSize)
		prefix, err := buffered.Peek(contentSniffSize)
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("reading archive content: %w", err)
		}
		format = detectContentPrefixFormat(prefix)
		if format == "" {
			return nil, fmt.Errorf("unsupported archive format: %s", filename)
		}
		content = buffered
	}

	raw, err := io.ReadAll(content)
	if err != nil {
		return nil, fmt.Errorf("reading archive content: %w", err)
	}

	return openRaw(format, raw)
}

// OpenBytes is like Open but accepts the archive content as a byte slice.
// The slice is retained (not copied) for the lifetime of the Reader and
// must not be modified by the caller after this call.
//
//nolint:ireturn // factory function returning interface by design
func OpenBytes(filename string, content []byte) (Reader, error) {
	format := detectFormat(filename)
	if format == "" {
		format = detectContentFormat(content)
	}
	if format == "" {
		return nil, fmt.Errorf("unsupported archive format: %s", filename)
	}

	return openRaw(format, content)
}

//nolint:ireturn
func openRaw(format string, raw []byte) (Reader, error) {
	switch format {
	case formatZIP:
		return openZip(raw)
	case formatTAR:
		return openTar(raw, "")
	case formatTarGzip, formatTGZ:
		return openTar(raw, "gzip")
	case formatTarBzip2:
		return openTar(raw, "bzip2")
	case formatTarXZ:
		return openTar(raw, "xz")
	case formatGem:
		return openGem(raw)
	default:
		return nil, fmt.Errorf("unsupported format: %s", format)
	}
}

func detectContentFormat(content []byte) string {
	return archiveFormat(magic.Detect(content).Format)
}

func detectContentPrefixFormat(content []byte) string {
	return archiveFormat(magic.DetectPrefix(content).Format)
}

func archiveFormat(detected string) string {
	switch detected {
	case "zip":
		return formatZIP
	case "tar":
		return formatTAR
	case "gzip":
		return formatTarGzip
	case "bzip2":
		return formatTarBzip2
	case "xz":
		return formatTarXZ
	default:
		return ""
	}
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
	return wrapPrefix(reader, stripPrefix), nil
}

// OpenBytesWithPrefix is like OpenWithPrefix but accepts the archive content as
// a byte slice. The slice is retained (not copied) for the lifetime of the
// Reader and must not be modified by the caller after this call.
//
//nolint:ireturn // factory function returning interface by design
func OpenBytesWithPrefix(filename string, content []byte, stripPrefix string) (Reader, error) {
	reader, err := OpenBytes(filename, content)
	if err != nil {
		return nil, err
	}
	return wrapPrefix(reader, stripPrefix), nil
}

//nolint:ireturn
func wrapPrefix(reader Reader, stripPrefix string) Reader {
	if stripPrefix == "" {
		return reader
	}
	return &prefixStripper{reader: reader, prefix: stripPrefix}
}

// detectFormat determines archive format from filename extension.
func detectFormat(filename string) string {
	filename = strings.ToLower(filename)

	// Check for compound extensions first
	if strings.HasSuffix(filename, ".tar.gz") {
		return formatTarGzip
	}
	if strings.HasSuffix(filename, ".tar.bz2") {
		return formatTarBzip2
	}
	if strings.HasSuffix(filename, ".tar.xz") {
		return formatTarXZ
	}

	// Check simple extensions. .apk is deliberately absent: Alpine packages
	// are gzipped tarballs and Android packages are zips, so it falls
	// through to content sniffing.
	ext := path.Ext(filename)
	switch ext {
	case ".zip", ".jar", ".whl", ".nupkg", ".egg", ".vsix":
		return formatZIP
	case ".tar":
		return formatTAR
	case ".tgz", ".crate":
		return formatTGZ
	case ".gem":
		return formatGem
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
