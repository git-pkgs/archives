package archives

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"
)

func TestBrowserList(t *testing.T) {
	for _, name := range []string{"test.tar", "test.tar.gz", "test.zip", "test.gem", "test.conda"} {
		for _, tc := range []struct {
			label   string
			names   []string
			options BrowseOptions
			prefix  string
		}{
			{"all entries", []string{"root/", "root/a", "root/src/b", "root/a"}, BrowseOptions{}, ""},
			{"common root", []string{"root/", "root/a", "root/src/b", "root/a"}, BrowseOptions{DetectRoot: true}, "root/"},
			{"explicit prefix", []string{"other/x", "package/a", "package/src/b"}, BrowseOptions{StripPrefix: "package/", DetectRoot: true}, "package/"},
			{"root file", []string{"README.md"}, BrowseOptions{DetectRoot: true}, ""},
			{"empty", nil, BrowseOptions{DetectRoot: true}, ""},
		} {
			t.Run(name+"/"+tc.label, func(t *testing.T) {
				data := streamTestArchive(t, name, browseTar(t, tc.names...))
				b, err := OpenBrowser(name, bytes.NewReader(data), func() (io.ReadCloser, error) {
					t.Error("List reopened its input")
					return nil, errors.New("unexpected reopen")
				}, tc.options)
				if err != nil {
					t.Fatal(err)
				}
				got, err := b.List()
				if err != nil {
					t.Fatal(err)
				}
				if len(tc.names) == 0 {
					if len(got) != 0 {
						t.Fatalf("empty archive listing = %+v", got)
					}
					return
				}
				assertFullListing(t, name, data, tc.prefix, got)
			})
		}
	}
}

func assertFullListing(t *testing.T, name string, data []byte, prefix string, got []FileInfo) {
	t.Helper()
	r, err := OpenBytesWithPrefix(name, data, prefix)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	want, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List = %+v, want %+v", got, want)
	}
}

func TestBrowserListLimits(t *testing.T) {
	data := browseTar(t, "root/a", "root/b")
	for _, limit := range []StreamOptions{
		{MaxEntries: 1}, {MaxInputBytes: 512}, {MaxEntryBytes: 1}, {MaxExpandedBytes: 1},
	} {
		b, err := OpenBrowser("test.tar", bytes.NewReader(data), nil, BrowseOptions{Limits: limit, DetectRoot: true})
		if err != nil {
			t.Fatal(err)
		}
		if files, err := b.List(); err == nil || files != nil {
			t.Fatalf("List returned partial result: %+v, %v", files, err)
		}
	}
}
