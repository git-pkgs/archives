package archives

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestCanceledOpen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	deadline, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	for _, ctx := range []context.Context{ctx, deadline} {
		data := browseTar(t, "README.md")
		input := &countStreamInput{Reader: bytes.NewReader(data)}
		_, err := OpenStreamContext(ctx, "test.tar", input, StreamOptions{})
		if !errors.Is(err, ctx.Err()) {
			t.Fatalf("OpenStreamContext: %v", err)
		}
		_, err = OpenStreamBytesContext(ctx, "test.tar", data, StreamOptions{})
		if !errors.Is(err, ctx.Err()) {
			t.Fatalf("OpenStreamBytesContext: %v", err)
		}
		_, err = OpenBrowserContext(ctx, "test.tar", input, nil, BrowseOptions{})
		if !errors.Is(err, ctx.Err()) {
			t.Fatalf("OpenBrowserContext: %v", err)
		}
		if input.bytes != 0 || input.closed {
			t.Fatalf("caller input touched: %+v", input)
		}
	}
}

func TestStreamCancellation(t *testing.T) {
	for _, name := range []string{"test.tar", "test.tar.gz", "test.tar.xz", "test.tar.zst", "test.zip", "test.gem", "test.conda"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			data := streamTestArchive(t, name, browseTar(t, "README.md", "other"))
			stream, err := OpenStreamBytesContext(ctx, name, data, StreamOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stream.Close() }()
			if _, err := stream.Next(); err != nil {
				t.Fatal(err)
			}
			cancel()
			if _, err := io.ReadAll(stream); !errors.Is(err, context.Canceled) {
				t.Fatalf("Read: %v", err)
			}
			if _, err := stream.Next(); !errors.Is(err, context.Canceled) {
				t.Fatalf("Next: %v", err)
			}
		})
	}
}

// Cancellation during a single operation must not depend on scheduler timing.
type cancelAfterChecks struct {
	context.Context
	cancel    context.CancelFunc
	remaining int
}

func (c *cancelAfterChecks) Err() error {
	if c.remaining > 0 {
		c.remaining--
		if c.remaining == 0 {
			c.cancel()
		}
	}
	return c.Context.Err()
}

func newCheckContext(t *testing.T) *cancelAfterChecks {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &cancelAfterChecks{Context: ctx, cancel: cancel}
}

func cancellationTar(t *testing.T) []byte {
	t.Helper()
	var data bytes.Buffer
	tw := tar.NewWriter(&data)
	writeTarFile(t, tw, "root/padding", strings.Repeat("padding", 100_000), 0o644)
	writeTarFile(t, tw, "root/README.md", "hello", 0o644)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestCancellationDuringSkippedEntry(t *testing.T) {
	for _, name := range []string{"test.tar", "test.tar.gz", "test.zip", "test.gem", "test.conda"} {
		t.Run(name, func(t *testing.T) {
			ctx := newCheckContext(t)
			stream, err := OpenStreamBytesContext(ctx, name, streamTestArchive(t, name, cancellationTar(t)), StreamOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stream.Close() }()
			if _, err := stream.Next(); err != nil {
				t.Fatal(err)
			}
			ctx.remaining = 20
			if _, err := stream.Next(); !errors.Is(err, context.Canceled) {
				t.Fatalf("skipped body: %v", err)
			}
		})
	}
}

func TestBrowserCancellation(t *testing.T) {
	for _, name := range []string{"test.tar.gz", "test.zip", "test.conda"} {
		for _, operation := range []string{"list", "directory", "extract"} {
			t.Run(name+"/"+operation, func(t *testing.T) {
				ctx := newCheckContext(t)
				data := streamTestArchive(t, name, cancellationTar(t))
				b, err := OpenBrowserContext(ctx, name, bytes.NewReader(data), nil, BrowseOptions{DetectRoot: true})
				if err != nil {
					t.Fatal(err)
				}
				ctx.remaining = 25
				switch operation {
				case "list":
					_, err = b.List()
				case "directory":
					_, err = b.ListDir("")
				case "extract":
					_, err = b.Extract("README.md")
				}
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("%s: %v", operation, err)
				}
			})
		}
	}
}

func TestBrowserCancellationClosesReopenedInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	data := browseTar(t, "README.md")
	first := &countStreamInput{Reader: bytes.NewReader(data)}
	reopened := &countStreamInput{Reader: bytes.NewReader(data)}
	b, err := OpenBrowserContext(ctx, "test.tar", first, func() (io.ReadCloser, error) {
		cancel()
		return reopened, nil
	}, BrowseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.List(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Extract("README.md"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if first.closed || !reopened.closed {
		t.Fatalf("first closed=%t, reopened closed=%t", first.closed, reopened.closed)
	}
}

func TestBrowserExtractedFileCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	data := streamTestArchive(t, "test.zip", browseTar(t, "README.md"))
	b, err := OpenBrowserContext(ctx, "test.zip", bytes.NewReader(data), nil, BrowseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	file, err := b.Extract("README.md")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	cancel()
	if _, err := io.ReadAll(file); !errors.Is(err, context.Canceled) {
		t.Fatalf("Read: %v", err)
	}
}
