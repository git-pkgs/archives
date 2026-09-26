//go:build tinygo

package archives

import (
	"errors"
	"fmt"
)

// ExtractAll returns an error wrapping errors.ErrUnsupported under TinyGo,
// which lacks os.Root.
func ExtractAll(r Reader, dir string, opts ...ExtractOption) error {
	return fmt.Errorf("archives: disk extraction not available on this target: %w", errors.ErrUnsupported)
}
