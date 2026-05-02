package archives

import (
	//nolint:gosec // md5 and sha1 are required for matching registry-published checksums
	"crypto/md5"
	//nolint:gosec // see above
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"strings"
)

// Supported hash algorithm names for Reader.Hash.
const (
	SHA256 = "sha256"
	SHA512 = "sha512"
	SHA1   = "sha1"
	MD5    = "md5"
)

// hashRaw computes the hex-encoded digest of data using the named algorithm.
func hashRaw(data []byte, algo string) (string, error) {
	var h hash.Hash

	switch strings.ToLower(algo) {
	case SHA256:
		h = sha256.New()
	case SHA512:
		h = sha512.New()
	case SHA1:
		//nolint:gosec // checksum verification, not security
		h = sha1.New()
	case MD5:
		//nolint:gosec // checksum verification, not security
		h = md5.New()
	default:
		return "", fmt.Errorf("unsupported hash algorithm: %s", algo)
	}

	h.Write(data)
	return hex.EncodeToString(h.Sum(nil)), nil
}
