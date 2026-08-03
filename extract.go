package archives

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// ErrUnsafePath is returned by ExtractAll when an archive entry name would
// resolve outside the target directory.
var ErrUnsafePath = errors.New("archive entry escapes target directory")

const (
	extractDirPerm  = 0o755
	extractFilePerm = 0o644
)

type deferredChmod struct {
	path string
	perm fs.FileMode
}

// ExtractAll writes every entry in r under dir, creating dir and any
// intermediate directories as needed. Entry permissions are preserved where
// the archive format records them; entries with no stored mode are written
// as 0644 (files) or 0755 (directories).
//
// All filesystem operations are confined to dir using os.Root, so a symlink
// under dir cannot redirect a write outside it. Entry names are additionally
// validated with filepath.Localize: absolute paths, names containing ".."
// elements that escape dir, and platform-invalid names cause ExtractAll to
// return ErrUnsafePath wrapping the offending entry name. Entries that the
// archive marks as non-regular (symlinks, devices) are skipped.
func ExtractAll(r Reader, dir string) error {
	if err := os.MkdirAll(dir, extractDirPerm); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	entries, err := r.List()
	if err != nil {
		return err
	}

	var dirModes []deferredChmod
	for _, entry := range entries {
		dm, err := extractEntry(r, root, entry)
		if err != nil {
			return err
		}
		if dm != nil {
			dirModes = append(dirModes, *dm)
		}
	}

	return applyDirModes(root, dirModes)
}

func extractEntry(r Reader, root *os.Root, entry FileInfo) (*deferredChmod, error) {
	name := path.Clean(strings.TrimSuffix(entry.Path, "/"))
	if name == "." || name == "" {
		return nil, nil
	}

	local, err := filepath.Localize(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", ErrUnsafePath, entry.Path)
	}

	if entry.IsDir {
		if err := root.MkdirAll(local, extractDirPerm); err != nil {
			return nil, err
		}
		if entry.HasMode {
			return &deferredChmod{path: local, perm: fs.FileMode(entry.Mode).Perm()}, nil
		}
		return nil, nil
	}

	if fs.FileMode(entry.Mode)&fs.ModeType != 0 {
		// Symlink, device, or other non-regular entry. Skip rather than
		// error so archives containing incidental symlinks still extract.
		return nil, nil
	}

	if parent := filepath.Dir(local); parent != "." {
		if err := root.MkdirAll(parent, extractDirPerm); err != nil {
			return nil, err
		}
	}

	src, err := r.Extract(entry.Path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = src.Close() }()

	// Perm() masks to the low nine bits, so setuid/setgid/sticky are dropped
	// deliberately: extracted registry archives are untrusted and should not
	// be able to plant privilege-escalating bits on disk.
	perm := fs.FileMode(extractFilePerm)
	if entry.HasMode {
		perm = fs.FileMode(entry.Mode).Perm()
	}
	// Remove any existing object at the destination so an in-root symlink,
	// a hard link to another inode, or a leftover file with different
	// permissions is replaced with a fresh regular file rather than
	// modified in place.
	if err := root.Remove(local); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	out, err := root.OpenFile(local, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close()
		return nil, fmt.Errorf("writing %s: %w", entry.Path, err)
	}
	if entry.HasMode {
		// The mode passed to OpenFile is subject to the process umask;
		// restore the recorded permissions on the open descriptor so no
		// path lookup is involved.
		if err := out.Chmod(perm); err != nil {
			_ = out.Close()
			return nil, err
		}
	}
	return nil, out.Close()
}

// applyDirModes sets recorded directory permissions after all entries have
// been written, deepest first, so restrictive parent modes cannot block
// chmod of their children. Each directory is opened through the root and
// chmod applied to the open descriptor to avoid the documented Root.Chmod
// symlink-replacement race.
func applyDirModes(root *os.Root, modes []deferredChmod) error {
	sort.Slice(modes, func(i, j int) bool {
		return depth(modes[i].path) > depth(modes[j].path)
	})
	for _, m := range modes {
		d, err := root.Open(m.path)
		if err != nil {
			return err
		}
		if err := d.Chmod(m.perm); err != nil {
			_ = d.Close()
			return err
		}
		if err := d.Close(); err != nil {
			return err
		}
	}
	return nil
}

func depth(p string) int {
	return strings.Count(p, string(filepath.Separator))
}
