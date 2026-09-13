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
	if err := checkZipEntryCount(raw); err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("opening conda zip: %w", err)
	}
	if err := checkArchiveEntryCount(len(zr.File)); err != nil {
		return nil, err
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
		entries, size, err := readCondaMember(raw, f, len(files))
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

func readCondaMember(raw []byte, f *zip.File, initialEntryCount int) ([]tarFileEntry, int64, error) {
	data, err := condaMemberBytes(raw, f)
	if err != nil {
		return nil, 0, err
	}

	tr, err := openTarWithInitialEntryCount(data, "zstd", initialEntryCount)
	if err != nil {
		return nil, 0, fmt.Errorf("opening %s: %w", f.Name, err)
	}
	var size int64
	for _, e := range tr.files {
		size += int64(len(e.data))
	}
	return tr.files, size, nil
}

// condaMemberBytes returns the raw bytes of a .conda zip member. Real
// .conda packages store members uncompressed (zip.Store), so DataOffset
// locates the payload inside raw and a slice returns it with no copy or
// buffer growth. This bypasses archive/zip's CRC32 check on the member;
// the inner zstd frame check catches corruption anyway. Non-Store members
// or out-of-range headers fall through to the original Open+ReadAll path.
func condaMemberBytes(raw []byte, f *zip.File) ([]byte, error) {
	if f.Method == zip.Store {
		size := f.CompressedSize64
		if off, err := f.DataOffset(); err == nil &&
			off >= 0 && size <= uint64(len(raw)) && off <= int64(len(raw))-int64(size) {
			return raw[off : off+int64(size)], nil
		}
	}

	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", f.Name, err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, maxDecompressedSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", f.Name, err)
	}
	if int64(len(data)) > maxDecompressedSize {
		return nil, fmt.Errorf("%w: %s exceeds %d bytes", ErrDecompressLimit, f.Name, maxDecompressedSize)
	}
	return data, nil
}
