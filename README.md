# archives

A Go library for reading and browsing archive files in memory. Supports ZIP, TAR (with gzip, bzip2, xz compression), and Ruby gem formats.

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

### Prefix stripping

Some package formats wrap content in a directory (npm uses `package/`). `OpenWithPrefix` strips a path prefix from all entries:

```go
reader, _ := archives.OpenWithPrefix("pkg.tgz", f, "package/")
// files are now accessible without the package/ prefix
```

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

- `.zip`, `.jar`, `.whl`, `.nupkg`, `.egg` (ZIP-based)
- `.tar`, `.tar.gz`, `.tgz`, `.tar.bz2`, `.tar.xz`
- `.gem` (Ruby gems with nested data.tar.gz)

## License

MIT
