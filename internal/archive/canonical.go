// Package archive produces and reads skill artifacts.
//
// The canonical artifact for multi-file skills is an uncompressed ustar
// stream written by this package's own header encoder (design §6):
//
//   - entries sorted by their header name in byte order; every parent
//     directory gets an explicit entry ("dir/") placed before its contents
//   - files mode 0644, directories 0755, uid/gid 0, empty uname/gname,
//     mtime 0, devmajor/devminor 0, no linkname
//   - no PAX or GNU extension headers; a path that cannot be expressed in the
//     ustar name (100) + prefix (155) fields is rejected
//   - terminated by exactly two 512-byte zero blocks (no record padding)
//
// The encoder does not depend on archive/tar's writer so the byte layout
// cannot drift with Go releases.
package archive

import (
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"path"
	"slices"
	"strings"

	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
)

const blockSize = 512

// ErrPathTooLong is returned when a path does not fit a ustar header.
var ErrPathTooLong = errors.New("path too long for canonical tar (no PAX headers are written)")

type entry struct {
	name string // header name; directories end with "/"
	dir  bool
	data []byte
}

// CanonicalTar encodes files as a canonical tar. File paths must already be
// valid (skill.ValidateFiles); duplicate paths are rejected.
func CanonicalTar(files []skill.File) ([]byte, error) {
	seen := map[string]bool{}
	dirs := map[string]bool{}
	var entries []entry
	for _, f := range files {
		if seen[f.Path] {
			return nil, fmt.Errorf("duplicate path %q", f.Path)
		}
		seen[f.Path] = true
		entries = append(entries, entry{name: f.Path, data: f.Data})
		for d := path.Dir(f.Path); d != "."; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	for d := range dirs {
		if seen[d] {
			return nil, fmt.Errorf("path %q is both a file and a directory", d)
		}
		entries = append(entries, entry{name: d + "/", dir: true})
	}
	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.name, b.name) })

	var buf bytes.Buffer
	for _, e := range entries {
		hdr, err := ustarHeader(e)
		if err != nil {
			return nil, err
		}
		buf.Write(hdr)
		if !e.dir {
			buf.Write(e.data)
			if pad := len(e.data) % blockSize; pad != 0 {
				buf.Write(make([]byte, blockSize-pad))
			}
		}
	}
	buf.Write(make([]byte, 2*blockSize))
	return buf.Bytes(), nil
}

func ustarHeader(e entry) ([]byte, error) {
	h := make([]byte, blockSize)
	name, prefix, err := splitUSTAR(e.name)
	if err != nil {
		return nil, err
	}
	copy(h[0:100], name)
	mode, typ, size := "0000644", byte('0'), int64(len(e.data))
	if e.dir {
		mode, typ, size = "0000755", '5', 0
	}
	if size >= 1<<33 {
		return nil, errors.New("file too large for ustar")
	}
	putField(h[100:108], mode)
	putField(h[108:116], "0000000") // uid
	putField(h[116:124], "0000000") // gid
	putField(h[124:136], fmt.Sprintf("%011o", size))
	putField(h[136:148], "00000000000") // mtime
	h[156] = typ
	copy(h[257:263], "ustar\x00")
	copy(h[263:265], "00")
	// uname [265:297] and gname [297:329] stay empty (all NUL).
	putField(h[329:337], "0000000") // devmajor
	putField(h[337:345], "0000000") // devminor
	copy(h[345:500], prefix)
	// Checksum: computed with the checksum field set to spaces, written as
	// six octal digits, NUL, space.
	for i := 148; i < 156; i++ {
		h[i] = ' '
	}
	var sum int
	for _, b := range h {
		sum += int(b)
	}
	copy(h[148:156], fmt.Sprintf("%06o\x00 ", sum))
	return h, nil
}

func putField(dst []byte, s string) {
	copy(dst, s)
	dst[len(s)] = 0
}

// splitUSTAR fits a name into name(100)/prefix(155). The split point is the
// last slash for which both parts fit, making the result deterministic.
func splitUSTAR(n string) (name, prefix string, err error) {
	if len(n) <= 100 {
		return n, "", nil
	}
	for i := len(n) - 1; i > 0; i-- {
		if n[i] != '/' || i == len(n)-1 {
			continue
		}
		p, rest := n[:i], n[i+1:]
		if len(p) <= 155 && len(rest) <= 100 && rest != "" {
			return rest, p, nil
		}
		if len(rest) > 100 {
			break
		}
	}
	return "", "", fmt.Errorf("%w: %q", ErrPathTooLong, n)
}

// StoredGzip wraps data in a gzip member that uses only stored (uncompressed)
// deflate blocks of at most 65535 bytes, a zero mtime, no file name, XFL 0 and
// OS 255. The output depends only on the input bytes, so its digest is stable
// across implementations. It lets clients that only accept .tar.gz archives
// (such as the skills CLI) install canonical tars with a reproducible digest;
// HTTP-level compression can still reduce transfer size.
func StoredGzip(data []byte) []byte {
	var buf bytes.Buffer
	buf.Grow(len(data) + len(data)/65535*5 + 32)
	buf.Write([]byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 0xff})
	rest := data
	for {
		n := min(len(rest), 65535)
		final := byte(0)
		if n == len(rest) {
			final = 1
		}
		buf.WriteByte(final) // BFINAL bit, BTYPE=00, padded to the byte boundary
		buf.Write([]byte{byte(n), byte(n >> 8), ^byte(n), ^byte(n >> 8)})
		buf.Write(rest[:n])
		rest = rest[n:]
		if final == 1 {
			break
		}
	}
	crc := crc32.ChecksumIEEE(data)
	size := uint32(len(data))
	buf.Write([]byte{byte(crc), byte(crc >> 8), byte(crc >> 16), byte(crc >> 24)})
	buf.Write([]byte{byte(size), byte(size >> 8), byte(size >> 16), byte(size >> 24)})
	return buf.Bytes()
}
