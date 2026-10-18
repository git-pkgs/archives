package archives

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
)

var ErrIsDir = errors.New("path is a directory")

var errStopBrowse = errors.New("stop browse")

// BrowseOptions controls per-pass limits and path prefixes.
type BrowseOptions struct {
	Limits StreamOptions
	// StripPrefix takes precedence over DetectRoot. Include the trailing slash.
	StripPrefix string
	// DetectRoot strips a directory shared by every entry.
	DetectRoot bool
}

// Browser lists directories and streams individual files without retaining
// expanded bodies. Finish and close each extracted file before the next call.
// A Browser is not safe for concurrent use.
type Browser struct {
	ctx      context.Context
	filename string
	options  BrowseOptions
	reopen   func() (io.ReadCloser, error)
	first    io.Reader
	data     []byte
}

// OpenBrowser prepares an archive for browsing. The caller owns content and
// must keep it open until the first operation and any extracted file finish.
// Later TAR and gem passes call reopen; the browser closes those readers.
// ZIP and conda input is buffered once and reused without calling reopen.
// A nil reopen is allowed, but a second sequential pass wraps errors.ErrUnsupported.
func OpenBrowser(filename string, content io.Reader, reopen func() (io.ReadCloser, error), options BrowseOptions) (*Browser, error) {
	return OpenBrowserContext(context.Background(), filename, content, reopen, options)
}

// OpenBrowserContext is OpenBrowser with cancellation across every pass,
// including decompression of buffered input. The reopen callback must honor ctx
// itself if opening can block. Canceling does not close the caller's input.
func OpenBrowserContext(ctx context.Context, filename string, content io.Reader, reopen func() (io.ReadCloser, error), options BrowseOptions) (*Browser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limits, err := options.Limits.defaults()
	if err != nil {
		return nil, err
	}
	options.Limits = limits
	format, content, err := streamFormat(filename, &streamInput{reader: contextReader{ctx: ctx, reader: content}, remaining: limits.MaxInputBytes})
	if err != nil {
		return nil, err
	}
	b := &Browser{ctx: ctx, filename: filename, options: options, reopen: reopen}
	if format != formatZIP && format != formatConda {
		b.first = content
		return b, nil
	}
	b.data, err = io.ReadAll(content)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (b *Browser) openStream() (*Stream, func() error, error) {
	if err := b.ctx.Err(); err != nil {
		return nil, nil, err
	}
	if b.data != nil {
		stream, err := OpenStreamBytesContext(b.ctx, b.filename, b.data, b.options.Limits)
		if err != nil {
			return nil, nil, err
		}
		return stream, stream.Close, nil
	}
	if b.first != nil {
		content := b.first
		b.first = nil
		stream, err := OpenStreamContext(b.ctx, b.filename, content, b.options.Limits)
		if err != nil {
			return nil, nil, err
		}
		return stream, stream.Close, nil
	}
	if b.reopen == nil {
		return nil, nil, fmt.Errorf("%w: reopening archive requires a callback", errors.ErrUnsupported)
	}
	content, err := b.reopen()
	if err != nil {
		return nil, nil, fmt.Errorf("reopening archive: %w", err)
	}
	stream, err := OpenStreamContext(b.ctx, b.filename, content, b.options.Limits)
	if err != nil {
		_ = content.Close()
		return nil, nil, err
	}
	return stream, func() error { return errors.Join(stream.Close(), content.Close()) }, nil
}

func (b *Browser) prefix() (string, error) {
	if b.options.StripPrefix != "" || !b.options.DetectRoot {
		return b.options.StripPrefix, nil
	}
	var root rootDetector
	err := b.walk(func(entry *StreamEntry) error {
		if !root.add(entry.FileInfo) {
			return errStopBrowse
		}
		return nil
	})
	return root.prefix(), err
}

func (b *Browser) entries() ([]FileInfo, string, error) {
	var entries []FileInfo
	err := b.walk(func(entry *StreamEntry) error {
		entries = append(entries, entry.FileInfo)
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	prefix := b.options.StripPrefix
	if prefix == "" && b.options.DetectRoot {
		prefix = CommonRoot(entries)
	}
	return entries, prefix, nil
}

// List returns all entries in archive order, retaining only metadata.
// Root detection and listing share one pass. Duplicate entries are preserved.
func (b *Browser) List() ([]FileInfo, error) {
	files, prefix, err := b.entries()
	if err != nil {
		return nil, err
	}
	if prefix == "" {
		return files, nil
	}
	return stripFilePrefix(files, prefix), nil
}

// ListDir returns immediate children in archive order, retaining only metadata.
// Root detection and listing share one pass. Duplicate files are preserved.
func (b *Browser) ListDir(dirPath string) ([]FileInfo, error) {
	entries, prefix, err := b.entries()
	if err != nil {
		return nil, err
	}
	files := listDir(entries, prefix+dirPath)
	if prefix == "" {
		return files, nil
	}
	return stripFilePrefix(files, prefix), nil
}

// Extract streams the first matching file. The caller must close the result.
// Root detection requires a separate pass, so sequential input needs reopen.
// Missing paths wrap fs.ErrNotExist; directories wrap ErrIsDir. Skipped entries
// count against limits. Entries after the target and trailers are not checked.
func (b *Browser) Extract(filePath string) (io.ReadCloser, error) {
	prefix, err := b.prefix()
	if err != nil {
		return nil, err
	}
	stream, release, err := b.openStream()
	if err != nil {
		return nil, err
	}
	for {
		entry, err := stream.Next()
		if err != nil {
			_ = release()
			if err == io.EOF {
				return nil, fmt.Errorf("%w: %s", fs.ErrNotExist, filePath)
			}
			return nil, err
		}
		if entry.Path != prefix+filePath {
			continue
		}
		if entry.IsDir {
			_ = release()
			return nil, fmt.Errorf("%w: %s", ErrIsDir, filePath)
		}
		return &browsedFile{Reader: stream, release: release}, nil
	}
}

type browsedFile struct {
	io.Reader
	release func() error
}

func (f *browsedFile) Close() error {
	if f.release == nil {
		return nil
	}
	release := f.release
	f.release = nil
	return release()
}

// CommonRoot returns the shared top-level directory, including its slash.
// Empty archives and archives containing top-level files have no common root.
func CommonRoot(files []FileInfo) string {
	var root rootDetector
	for _, file := range files {
		if !root.add(file) {
			break
		}
	}
	return root.prefix()
}

type rootDetector struct {
	root  string
	mixed bool
}

func (d *rootDetector) add(file FileInfo) bool {
	if d.mixed {
		return false
	}
	dir, _, nested := strings.Cut(file.Path, "/")
	if dir == "" || (!nested && !file.IsDir) {
		d.mixed = true
		return false
	}
	if d.root == "" {
		d.root = dir
	} else if dir != d.root {
		d.mixed = true
	}
	return !d.mixed
}

func (d *rootDetector) prefix() string {
	if d.mixed || d.root == "" {
		return ""
	}
	return d.root + "/"
}
