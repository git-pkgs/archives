package archives

import (
	"errors"
	"io"
	"strings"
)

// walk calls fn for each entry in archive order until fn returns an error or
// the archive ends. fn may read the entry body from the stream.
func (a *Browser) walk(fn func(entry *StreamEntry) error) error {
	stream, release, err := a.openStream()
	if err != nil {
		return err
	}
	defer func() { _ = release() }()

	for {
		entry, err := stream.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(entry); err != nil {
			if errors.Is(err, errStopBrowse) {
				return nil
			}
			return err
		}
	}
}

func listDir(entries []FileInfo, dirPath string) []FileInfo {
	dirPath = normalizeDir(dirPath)
	var files []FileInfo
	seenDirs := make(map[string]bool)

	for _, f := range entries {
		p := f.Path

		if isInDir(p, dirPath) {
			if f.IsDir {
				name := strings.TrimSuffix(strings.TrimPrefix(p, dirPath), "/")
				if seenDirs[name] {
					continue
				}
				seenDirs[name] = true
			}
			files = append(files, f)
			continue
		}

		if dirPath != "" && !strings.HasPrefix(p, dirPath) {
			continue
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(p, dirPath), "/")
		name, _, nested := strings.Cut(rel, "/")
		if nested && !seenDirs[name] {
			seenDirs[name] = true
			files = append(files, FileInfo{
				Path:  dirPath + name + "/",
				Name:  name,
				IsDir: true,
			})
		}
	}

	return files
}
