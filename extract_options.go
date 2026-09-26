package archives

import "errors"

const (
	extractDirPerm  = 0o755
	extractFilePerm = 0o644
)

// ErrUnsafePath is returned by ExtractAll when an archive entry name would
// resolve outside the target directory.
var ErrUnsafePath = errors.New("archive entry escapes target directory")

// ErrExtractLimit is returned by ExtractAll when the total decompressed bytes
// written would exceed the WithMaxBytes limit.
var ErrExtractLimit = errors.New("extracted bytes exceed limit")

type extractConfig struct {
	maxBytes int64
}

// ExtractOption configures ExtractAll.
type ExtractOption func(*extractConfig)

// WithMaxBytes caps the total number of decompressed bytes ExtractAll will
// write. The limit is enforced against bytes actually read from each entry,
// not header-declared sizes, so an archive whose headers under-report content
// still cannot exceed it. A value of zero or less disables the limit.
func WithMaxBytes(n int64) ExtractOption {
	return func(c *extractConfig) { c.maxBytes = n }
}
