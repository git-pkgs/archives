# archives

A Go library for reading and browsing archive files in memory. Supports ZIP, TAR (with gzip, bzip2, xz, zstd compression), and Ruby gem formats.

## Installation

```bash
go get github.com/git-pkgs/archives
```

## Usage

```go
package main

import (
	"fmt"
	"os"

	"github.com/git-pkgs/archives"
)

func main() {
	f, _ := os.Open("package.tar.gz")
	defer f.Close()

	reader, _ := archives.Open("package.tar.gz", f)
	defer reader.Close()

	// List all files
	files, _ := reader.List()
	for _, fi := range files {
		fmt.Println(fi.Path, fi.Size)
	}

	// List a specific directory
	dirFiles, _ := reader.ListDir("src")
	for _, fi := range dirFiles {
		fmt.Println(fi.Name, fi.IsDir)
	}

	// Extract a file
	rc, _ := reader.Extract("README.md")
	defer rc.Close()
	// read from rc...
}
```

### Hashing

`Open` buffers the raw archive bytes in memory, so the reader can compute checksums of the original artifact without re-reading from the source. This is useful for verifying a downloaded package against the digest published by its registry.

```go
reader, _ := archives.Open("rails-7.1.0.gem", f)
defer reader.Close()

sha, _ := reader.Hash(archives.SHA256)
fmt.Println(sha) // hex-encoded sha256 of the .gem file

// also available: archives.SHA512, archives.SHA1, archives.MD5
```

The hash is computed over the archive as it was passed to `Open`, not the decompressed contents. For nested formats like gems this means the outer `.gem` file, which is what rubygems.org publishes. If you already have the bytes in hand, `OpenBytes` skips the extra read:

```go
reader, _ := archives.OpenBytes("pkg.tgz", data)
```

Filename mappings are used first. When a name has no supported extension,
`Open` and `OpenBytes` detect ZIP, TAR, gzip, bzip2, xz, and zstd from the content.
Compressed content is opened as TAR and returns a parser error when it does not
contain a TAR archive. `Open` reads at most 512 bytes before rejecting an
unsupported stream with no recognised extension.

### Prefix stripping

Some package formats wrap content in a directory (npm uses `package/`). `OpenWithPrefix` strips a path prefix from all entries:

```go
reader, _ := archives.OpenWithPrefix("pkg.tgz", f, "package/")
// files are now accessible without the package/ prefix
```

### Extracting to disk

`ExtractAll` writes every entry under a target directory, creating it and any intermediate directories. Entry names are validated with `filepath.Localize` so absolute paths and `..` segments that would escape the target return `ErrUnsafePath` naming the offending entry.

```go
reader, _ := archives.Open("pkg.whl", f)
defer reader.Close()

if err := archives.ExtractAll(reader, dir); err != nil {
    return err
}
```

File permissions are preserved where the archive records them. Entries that the format marks as symlinks or other non-regular types are skipped.

### Comparing versions

The `diff` subpackage compares two archives and produces unified diffs. It classifies each file as added, deleted, modified, or binary, and includes line-level diff output for text files.

```go
import "github.com/git-pkgs/archives/diff"

result, _ := diff.Compare(oldReader, newReader)
for _, f := range result.Files {
	fmt.Printf("%s %s (+%d -%d)\n", f.Type, f.Path, f.LinesAdded, f.LinesDeleted)
	if f.Diff != "" {
		fmt.Println(f.Diff)
	}
}
```

## Supported formats

- `.zip`, `.jar`, `.whl`, `.nupkg`, `.egg`, `.vsix` (ZIP-based)
- `.tar`, `.tar.gz`, `.tgz`, `.crate`, `.tar.bz2`, `.tar.xz`, `.tar.zst`
- `.gem` (Ruby gems with nested data.tar.gz)
- `.apk` (routed by content: Android packages open as ZIP, Alpine packages open as gzipped tar)

Filenames without a recognised extension are opened by inspecting the first bytes for a ZIP, tar, gzip, bzip2, xz, or zstd signature.

## License

MIT
