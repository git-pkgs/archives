package archives

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
)

const hexContents = "contents.tar.gz"

// Hex packages have four regular outer entries, in any order.
func isHex(content io.Reader, options StreamOptions) (bool, error) {
	tr := tar.NewReader(content)
	budget := Stream{options: containerOptions(options)}
	var limitErr error
	remaining := map[string]bool{"VERSION": true, "CHECKSUM": true, "metadata.config": true, hexContents: true}
	for len(remaining) != 0 {
		h, err := tr.Next()
		if err != nil || !remaining[h.Name] || h.Typeflag != tar.TypeReg {
			return false, nil
		}
		if limitErr == nil {
			limitErr = budget.checkEntry(&StreamEntry{FileInfo: fileInfoFromTar(h)})
			if limitErr == nil {
				budget.entries++
				budget.expanded += h.Size
			}
		}
		switch h.Name {
		case "VERSION":
			if h.Size != 1 {
				return false, nil
			}
			var version [1]byte
			if _, err := io.ReadFull(tr, version[:]); err != nil || version[0] != '3' {
				return false, nil
			}
		case "CHECKSUM":
			if h.Size != int64(hex.EncodedLen(sha256.Size)) {
				return false, nil
			}
			checksum, err := io.ReadAll(tr)
			if err != nil || string(checksum) != strings.ToUpper(string(checksum)) {
				return false, nil
			}
			if _, err := hex.DecodeString(string(checksum)); err != nil {
				return false, nil
			}
		}
		delete(remaining, h.Name)
	}
	return true, limitErr
}

type plainTarStream struct {
	ctx     context.Context
	content io.Reader
	options StreamOptions
	source  streamSource
}

func (s *plainTarStream) Next() (*StreamEntry, error) {
	if s.source == nil {
		probe := &hexProbe{reader: s.content}
		hexPackage, err := isHex(probe, s.options)
		if err != nil {
			return nil, err
		}
		input := io.MultiReader(bytes.NewReader(probe.prefix.Bytes()), &probeTail{reader: s.content, err: probe.err})
		s.content = nil
		if hexPackage {
			s.source = &nestedTarStream{outer: newContainerTar(s.ctx, input, s.options), member: hexContents, kind: "Hex"}
		} else {
			var err error
			s.source, err = newTarStream(s.ctx, input, "")
			if err != nil {
				return nil, err
			}
		}
	}
	return s.source.Next()
}

func (s *plainTarStream) Read(p []byte) (int, error) {
	if s.source == nil {
		return 0, io.EOF
	}
	return s.source.Read(p)
}

func (s *plainTarStream) Close() error {
	s.content = nil
	if s.source != nil {
		return s.source.Close()
	}
	return nil
}

// Preserve probe errors as well as bytes so a failed probe cannot hide input errors.
type hexProbe struct {
	reader io.Reader
	prefix bytes.Buffer
	err    error
}

func (p *hexProbe) Read(b []byte) (int, error) {
	n, err := p.reader.Read(b)
	_, _ = p.prefix.Write(b[:n])
	p.err = err
	return n, err
}

type probeTail struct {
	reader io.Reader
	err    error
}

func (p *probeTail) Read(b []byte) (int, error) {
	if p.err != nil {
		return 0, p.err
	}
	return p.reader.Read(b)
}
