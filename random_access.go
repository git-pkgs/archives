package archives

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
)

// OpenStreamReaderAt opens ZIP or conda input without buffering the archive.
// size is the complete compressed input size. The caller owns content and must
// keep it open and unchanged until the stream is closed. Unread ZIP entries are
// skipped without decoding; their declared sizes still count against limits.
func OpenStreamReaderAt(filename string, content io.ReaderAt, size int64, options StreamOptions) (*Stream, error) {
	return OpenStreamReaderAtContext(context.Background(), filename, content, size, options)
}

// OpenStreamReaderAtContext is OpenStreamReaderAt with cancellation before and
// after each source read. Cancellation does not close or interrupt the input.
func OpenStreamReaderAtContext(ctx context.Context, filename string, content io.ReaderAt, size int64, options StreamOptions) (*Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	options, err := options.defaults()
	if err != nil {
		return nil, err
	}
	if size < 0 {
		return nil, fmt.Errorf("negative archive size: %d", size)
	}
	if size > options.MaxInputBytes {
		return nil, ErrInputLimit
	}
	input := contextReaderAt{ctx: ctx, reader: content}
	format, _, err := streamFormat(filename, io.NewSectionReader(input, 0, size))
	if err != nil {
		return nil, err
	}
	if format != formatZIP && format != formatConda {
		return nil, fmt.Errorf("%w: random-access archive format: %s", errors.ErrUnsupported, filename)
	}
	if err := checkZipEntryCountLimitContext(ctx, input, size, options.MaxEntries); err != nil {
		return nil, err
	}
	reader, err := zip.NewReader(input, size)
	if err != nil {
		return nil, fmt.Errorf("opening zip: %w", err)
	}
	if len(reader.File) > options.MaxEntries {
		return nil, ErrEntryLimit
	}
	reader.RegisterDecompressor(zip.Deflate, newFlateReader)
	var source streamSource = &zipStream{ctx: ctx, reader: reader, skipUnread: true}
	if format == formatConda {
		source = &condaStream{outer: &Stream{ctx: ctx, source: source, options: containerOptions(options)}}
	}
	return &Stream{ctx: ctx, source: source, options: options}, nil
}

// OpenBrowserReaderAt prepares ZIP or conda input for repeated browsing without
// copying it. The caller owns content and must keep it open and unchanged until
// browsing and all extracted files finish. Limits apply to each pass. This API
// does not implement Reader or whole-artifact hashing; hash the source separately.
func OpenBrowserReaderAt(filename string, content io.ReaderAt, size int64, options BrowseOptions) (*Browser, error) {
	return OpenBrowserReaderAtContext(context.Background(), filename, content, size, options)
}

// OpenBrowserReaderAtContext is OpenBrowserReaderAt with cancellation across
// every pass, including reads from extracted files.
func OpenBrowserReaderAtContext(ctx context.Context, filename string, content io.ReaderAt, size int64, options BrowseOptions) (*Browser, error) {
	stream, err := OpenStreamReaderAtContext(ctx, filename, content, size, options.Limits)
	if err != nil {
		return nil, err
	}
	if err := stream.Close(); err != nil {
		return nil, err
	}
	return &Browser{ctx: ctx, filename: filename, options: options, random: content, size: size}, nil
}
