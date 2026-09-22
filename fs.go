package archives

import (
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"
)

const (
	fsOpNew     = "newfs"
	fsOpRead    = "read"
	fsOpReadDir = "readdir"
)

// FS is a read-only filesystem over a Reader. The Reader must remain open
// while the filesystem and its files are in use.
type FS struct {
	reader Reader
	nodes  map[string]*fsNode
}

type fsNode struct {
	info     archiveFileInfo
	children []fs.DirEntry
}

var (
	_ fs.ReadDirFS   = (*FS)(nil)
	_ fs.ReadFileFS  = (*FS)(nil)
	_ fs.StatFS      = (*FS)(nil)
	_ fs.ReadDirFile = (*archiveFile)(nil)
)

// NewFS indexes r without extracting file contents. Missing parent directories
// are created in the index. Leading "./" and directory trailing slashes are
// removed from archive names; other invalid fs paths and file/directory
// conflicts return fs.ErrInvalid. The first entry for each path is used.
// Symlinks and other special entries can be listed, but cannot be opened.
// Closing an opened file does not close r; the caller owns the Reader.
func NewFS(r Reader) (*FS, error) {
	entries, err := r.List()
	if err != nil {
		return nil, &fs.PathError{Op: fsOpNew, Path: ".", Err: err}
	}
	f := &FS{reader: r, nodes: make(map[string]*fsNode, len(entries))}
	for _, entry := range entries {
		if err := f.addEntry(entry); err != nil {
			return nil, err
		}
	}
	if err := f.addDirectories(); err != nil {
		return nil, err
	}
	for name, node := range f.nodes {
		if name != "." {
			parent := f.nodes[path.Dir(name)]
			parent.children = append(parent.children, fs.FileInfoToDirEntry(node.info))
		}
	}
	for _, node := range f.nodes {
		slices.SortFunc(node.children, func(a, b fs.DirEntry) int {
			return strings.Compare(a.Name(), b.Name())
		})
	}
	return f, nil
}

func (f *FS) addEntry(entry FileInfo) error {
	name := entry.Path
	if entry.IsDir {
		name = strings.TrimSuffix(name, "/")
	}
	for strings.HasPrefix(name, "./") {
		name = strings.TrimPrefix(name, "./")
	}
	if !fs.ValidPath(name) || (name == "." && !entry.IsDir) {
		return &fs.PathError{Op: fsOpNew, Path: entry.Path, Err: fs.ErrInvalid}
	}
	if previous, ok := f.nodes[name]; ok {
		if previous.info.IsDir() != entry.IsDir {
			return &fs.PathError{Op: fsOpNew, Path: entry.Path, Err: fs.ErrInvalid}
		}
		return nil
	}
	f.nodes[name] = &fsNode{info: archiveFileInfo{entry: entry, name: path.Base(name)}}
	return nil
}

func (f *FS) addDirectories() error {
	if _, ok := f.nodes["."]; !ok {
		f.nodes["."] = implicitDirectory(".")
	}
	for name := range f.nodes {
		for parent := path.Dir(name); ; parent = path.Dir(parent) {
			if node, ok := f.nodes[parent]; ok {
				if !node.info.IsDir() {
					return &fs.PathError{Op: fsOpNew, Path: parent, Err: fs.ErrInvalid}
				}
				break
			}
			f.nodes[parent] = implicitDirectory(path.Base(parent))
		}
	}
	return nil
}

func implicitDirectory(name string) *fsNode {
	return &fsNode{info: archiveFileInfo{name: name, entry: FileInfo{IsDir: true}}}
}

func (f *FS) lookup(op, name string) (*fsNode, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	node, ok := f.nodes[name]
	if !ok {
		return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
	}
	return node, nil
}

// Open opens a file or directory. Names follow fs.ValidPath, with "." as root.
//
//nolint:ireturn // fs.FS requires this return type
func (f *FS) Open(name string) (fs.File, error) {
	node, err := f.lookup("open", name)
	if err != nil {
		return nil, err
	}
	file := &archiveFile{name: name, node: node}
	if node.info.IsDir() {
		return file, nil
	}
	if !node.info.Mode().IsRegular() {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	file.content, err = f.reader.Extract(node.info.entry.Path)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return file, nil
}

// Stat returns metadata for a file or directory without extracting its contents.
//
//nolint:ireturn // fs.StatFS requires this return type
func (f *FS) Stat(name string) (fs.FileInfo, error) {
	node, err := f.lookup("stat", name)
	if err != nil {
		return nil, err
	}
	return node.info, nil
}

// ReadDir returns directory entries sorted by name.
func (f *FS) ReadDir(name string) ([]fs.DirEntry, error) {
	node, err := f.lookup(fsOpReadDir, name)
	if err != nil {
		return nil, err
	}
	if !node.info.IsDir() {
		return nil, &fs.PathError{Op: fsOpReadDir, Path: name, Err: fs.ErrInvalid}
	}
	return append([]fs.DirEntry{}, node.children...), nil
}

// ReadFile reads the named file's contents.
func (f *FS) ReadFile(name string) ([]byte, error) {
	file, err := f.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return io.ReadAll(file)
}

type archiveFileInfo struct {
	entry FileInfo
	name  string
}

func (i archiveFileInfo) Name() string       { return i.name }
func (i archiveFileInfo) Size() int64        { return i.entry.Size }
func (i archiveFileInfo) ModTime() time.Time { return i.entry.ModTime }
func (i archiveFileInfo) IsDir() bool        { return i.entry.IsDir }
func (i archiveFileInfo) Sys() any           { return nil }

func (i archiveFileInfo) Mode() fs.FileMode {
	mode := fs.FileMode(i.entry.Mode)
	if !i.entry.HasMode {
		mode = mode.Type() | extractFilePerm
		if i.IsDir() {
			mode = mode.Type() | extractDirPerm
		}
	}
	if i.IsDir() {
		mode |= fs.ModeDir
	}
	return mode
}

type archiveFile struct {
	name    string
	node    *fsNode
	content io.ReadCloser
	offset  int
	closed  bool
}

//nolint:ireturn // fs.File requires this return type
func (f *archiveFile) Stat() (fs.FileInfo, error) {
	if f.closed {
		return nil, &fs.PathError{Op: "stat", Path: f.name, Err: fs.ErrClosed}
	}
	return f.node.info, nil
}

func (f *archiveFile) Read(b []byte) (int, error) {
	if f.closed {
		return 0, &fs.PathError{Op: fsOpRead, Path: f.name, Err: fs.ErrClosed}
	}
	if f.node.info.IsDir() {
		return 0, &fs.PathError{Op: fsOpRead, Path: f.name, Err: fs.ErrInvalid}
	}
	n, err := f.content.Read(b)
	if err != nil && err != io.EOF {
		err = &fs.PathError{Op: fsOpRead, Path: f.name, Err: err}
	}
	return n, err
}

func (f *archiveFile) Close() error {
	if f.closed {
		return &fs.PathError{Op: "close", Path: f.name, Err: fs.ErrClosed}
	}
	f.closed = true
	if f.content != nil {
		if err := f.content.Close(); err != nil {
			return &fs.PathError{Op: "close", Path: f.name, Err: err}
		}
	}
	return nil
}

func (f *archiveFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if f.closed {
		return nil, &fs.PathError{Op: fsOpReadDir, Path: f.name, Err: fs.ErrClosed}
	}
	if !f.node.info.IsDir() {
		return nil, &fs.PathError{Op: fsOpReadDir, Path: f.name, Err: fs.ErrInvalid}
	}
	remaining := f.node.children[f.offset:]
	if n > 0 && len(remaining) == 0 {
		return nil, io.EOF
	}
	if n > 0 && n < len(remaining) {
		remaining = remaining[:n]
	}
	f.offset += len(remaining)
	return append([]fs.DirEntry{}, remaining...), nil
}
