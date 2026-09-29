# archives

A Go library for reading and browsing archive files in memory. Supports ZIP, TAR (with gzip, bzip2, xz, zstd compression), Ruby gem, and conda formats.

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

### Sequential reading

`OpenStream` reads entries one at a time through `Next` and `Read`, without
retaining expanded file bodies. It supports every format listed below,
including the inner files of gem and conda packages.

```go
stream, err := archives.OpenStream("package.tgz", f, archives.StreamOptions{
    MaxInputBytes:    64 << 20,
    MaxEntryBytes:     8 << 20,
    MaxExpandedBytes: 64 << 20,
    MaxEntries:       2000,
})
if err != nil {
    return err
}
defer stream.Close()

for {
    entry, err := stream.Next()
    if err == io.EOF {
        break
    }
    if err != nil {
        return err
    }
    fmt.Println(entry.Path, entry.Size)
    if _, err := io.Copy(io.Discard, stream); err != nil {
        return err
    }
}
```

Import `io` for this example. `Next` discards unread entry data, and skipped
entries still count towards the limits. Entries remain in archive order,
including duplicates. Each entry's `Read` ends at `io.EOF`; iteration errors
are terminal. The caller owns the input, and `Close` releases decoder resources
without closing or draining it. A TAR end marker ends iteration, so trailing
data and compression trailers beyond that marker may remain unchecked.

ZIP and conda require random access to their compressed input. `OpenStream`
buffers that input within `MaxInputBytes`; `OpenStreamBytes(name, data, options)`
reuses an existing byte slice without copying it. Keep that slice unchanged
until `Close`. TAR and gem can consume a forward-only input directly.

Zero limits default to 512 MiB for each byte limit and 100,000 entries.
Negative limits are rejected. Entry and expanded-byte limits use logical
header sizes, including sparse files, before exposing a body. Container members
in gem and conda have a separate entry-count budget, with their combined bodies
bounded by `MaxInputBytes`. Decoder workspace and metadata are additional memory;
these limits do not cap total heap usage. For forward-only input, the input
limit covers bytes consumed, with at most one extra byte read to detect overflow.

For `swhid-go`, regular files can go directly to
`objects.ComputeContentHashReader(stream, entry.Size)`. This preserves its
collision-detecting hash without buffering a file. `StreamEntry` includes the
mode bits, `Linkname`, and `IsHardlink`: TAR symlink targets are in `Linkname`,
while ZIP symlink targets are in the body. A directory-hashing consumer must
retain paths, modes, and content hashes, resolve hard links, and apply its own
duplicate-path policy. The stream cannot rewind to hash the whole artifact;
hash the original byte slice or use a tee on the input and consume it fully.

### Prefix stripping

Some package formats wrap content in a directory (npm uses `package/`). `OpenWithPrefix` strips a path prefix from all entries:

```go
reader, _ := archives.OpenWithPrefix("pkg.tgz", f, "package/")
// files are now accessible without the package/ prefix
```

### Using io/fs

`NewFS` adapts a reader to `fs.FS`, with `ReadDir`, `ReadFile`, and `Stat` support. It indexes metadata once and reads file contents through the reader as needed.

```go
archiveFS, err := archives.NewFS(reader)
if err != nil {
    return err
}
data, err := fs.ReadFile(archiveFS, "lib/util.js")
if err != nil {
    return err
}
fmt.Println(string(data))
if err := fs.WalkDir(archiveFS, ".", func(name string, entry fs.DirEntry, err error) error {
    if err != nil {
        return err
    }
    fmt.Println(name)
    return nil
}); err != nil {
    return err
}
```

Import `io/fs` for these functions. Paths use `/` separators and `"."` for the root, and missing parent directories appear automatically. Archive names may have leading `./` or directory trailing slashes; other invalid paths and file/directory conflicts cause `NewFS` to return `fs.ErrInvalid`. Duplicate paths use the first entry. Symlinks and other special entries appear in listings but cannot be opened.

Keep the reader open while using the filesystem. Closing an individual file leaves the reader open.

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

Under TinyGo, `ExtractAll` returns an error matching `errors.ErrUnsupported` because `os.Root` is unavailable. In-memory reading through `Reader.Extract` remains available.

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
- `.conda` (v2 conda packages: zip of `pkg-*.tar.zst` and `info-*.tar.zst`, presented as one merged tar)
- `.apk` (routed by content: Android packages open as ZIP, Alpine packages open as gzipped tar)

Filenames without a recognised extension are opened by inspecting the first bytes for a ZIP, tar, gzip, bzip2, xz, or zstd signature.

## License

[MIT](LICENSE).
