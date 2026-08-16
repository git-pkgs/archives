package archives

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"strings"
)

// openConda handles the v2 .conda format used by anaconda.org and
// conda-forge: an uncompressed zip containing metadata.json plus two
// zstd-compressed tarballs, pkg-<name>.tar.zst holding the installed file
// tree and info-<name>.tar.zst holding index.json, paths.json and the
// recipe. Both tarballs already store their entries with the paths that
// appear in the equivalent v1 .tar.bz2 package (info/ is a prefix inside
// the tar, not something to add), so the reader presents them as a single
// merged tarReader with raw pointing at the outer .conda bytes so Hash
// matches the digest anaconda.org publishes in repodata.json.
func openConda(raw []byte) (*tarReader, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("opening conda zip: %w", err)
	}

	var files []tarFileEntry
	var total int64
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, ".tar.zst") {
			continue
		}
		if !strings.HasPrefix(f.Name, "pkg-") && !strings.HasPrefix(f.Name, "info-") {
			continue
		}
		entries, size, err := readCondaMember(f)
		if err != nil {
			return nil, err
		}
		total += size
		if total > maxDecompressedSize {
			return nil, fmt.Errorf("%w: exceeds %d bytes", ErrDecompressLimit, maxDecompressedSize)
		}
		files = append(files, entries...)
	}
	if files == nil {
		return nil, fmt.Errorf("no pkg-*.tar.zst or info-*.tar.zst member in conda package")
	}

	index := make(map[string]int, len(files))
	for i, f := range files {
		if _, seen := index[f.info.Path]; !seen {
			index[f.info.Path] = i
		}
	}

	return &tarReader{raw: raw, files: files, index: index}, nil
}

func readCondaMember(f *zip.File) ([]tarFileEntry, int64, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, 0, fmt.Errorf("opening %s: %w", f.Name, err)
	}
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(io.LimitReader(rc, maxDecompressedSize+1))
	if err != nil {
		return nil, 0, fmt.Errorf("reading %s: %w", f.Name, err)
	}
	if int64(len(data)) > maxDecompressedSize {
		return nil, 0, fmt.Errorf("%w: %s exceeds %d bytes", ErrDecompressLimit, f.Name, maxDecompressedSize)
	}

	tr, err := openTar(data, "zstd")
	if err != nil {
		return nil, 0, fmt.Errorf("opening %s: %w", f.Name, err)
	}
	var size int64
	for _, e := range tr.files {
		size += int64(len(e.data))
	}
	return tr.files, size, nil
}
