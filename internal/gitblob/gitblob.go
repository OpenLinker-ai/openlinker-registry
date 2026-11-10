// Package gitblob computes git object identifiers for blob contents.
//
// The hash is always computed over the exact raw bytes: no line-ending or
// encoding normalization is applied, so the result matches what git stores.
package gitblob

import (
	"crypto/sha1" //nolint:gosec // git object ids are SHA-1 by definition; used for integrity matching, not security.
	"encoding/hex"
	"strconv"
)

// SHA1 returns the lowercase hex git blob id of data ("blob <len>\x00" + data).
func SHA1(data []byte) string {
	h := sha1.New() //nolint:gosec
	h.Write([]byte("blob " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
