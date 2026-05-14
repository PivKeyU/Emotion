package importer

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// STRM represents the parsed content of a .strm file.
//
// STRM files are simple text pointers Emby/Kodi treat as virtual videos.
// In the wild they come in several flavors:
//
//  1. Single URL (most common):
//     https://cdn.example.com/movie.mkv
//
//  2. URL with query params / signed URLs (cloud drives):
//     https://pan.example.com/redirect?fid=123&sign=abc
//
//  3. plugin:// URLs (Kodi-only); we treat as opaque URL.
//
//  4. Local absolute or relative paths:
//     /mnt/media/movies/foo.mkv
//     ../downloads/bar.mp4
//
//  5. Multi-line with comments (primary first, fallback after):
//     # preferred source
//     https://primary.example.com/movie.mkv
//     https://fallback.example.com/movie.mkv
//
// We capture every non-empty, non-comment line as Targets, and the first as Primary.
type STRM struct {
	// Primary is the first playable target (URL or filesystem path).
	Primary string
	// Targets includes every non-empty, non-comment line in order.
	Targets []string
	// IsURL reports whether Primary looks like a URL rather than a filesystem path.
	IsURL bool
}

// ParseSTRM reads a .strm file from disk. Relative paths are resolved against
// the strm file's directory.
func ParseSTRM(path string) (*STRM, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := ParseSTRMBytes(data)

	if !s.IsURL && s.Primary != "" && !filepath.IsAbs(s.Primary) {
		s.Primary = filepath.Join(filepath.Dir(path), s.Primary)
	}
	for i, t := range s.Targets {
		if !looksLikeURL(t) && !filepath.IsAbs(t) {
			s.Targets[i] = filepath.Join(filepath.Dir(path), t)
		}
	}
	return s, nil
}

// ParseSTRMBytes parses already-read bytes.
func ParseSTRMBytes(data []byte) *STRM {
	data = normalizeSTRMBytes(data)
	data = stripBOM(data)
	s := &STRM{}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(strings.TrimSpace(raw), "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") || strings.HasPrefix(line, ";") {
			continue
		}
		s.Targets = append(s.Targets, line)
	}
	if len(s.Targets) > 0 {
		s.Primary = s.Targets[0]
		s.IsURL = looksLikeURL(s.Primary)
	}
	return s
}

func normalizeSTRMBytes(data []byte) []byte {
	switch {
	case len(data) >= 2 && data[0] == 0xff && data[1] == 0xfe:
		return decodeUTF16(data[2:], true)
	case len(data) >= 2 && data[0] == 0xfe && data[1] == 0xff:
		return decodeUTF16(data[2:], false)
	case utf8.Valid(data):
		return data
	case bytes.IndexByte(data, 0) >= 0:
		return decodeUTF16(data, true)
	default:
		return data
	}
}

func decodeUTF16(data []byte, littleEndian bool) []byte {
	if len(data)%2 == 1 {
		data = data[:len(data)-1]
	}
	u16 := make([]uint16, 0, len(data)/2)
	for i := 0; i+1 < len(data); i += 2 {
		if littleEndian {
			u16 = append(u16, uint16(data[i])|uint16(data[i+1])<<8)
		} else {
			u16 = append(u16, uint16(data[i])<<8|uint16(data[i+1]))
		}
	}
	return []byte(string(utf16.Decode(u16)))
}

// looksLikeURL is true if the value parses to a URL with a scheme we recognize.
// STRM files use schemes like http, https, ftp, plugin, smb, nfs, rtsp, rtmp.
func looksLikeURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" {
		return false
	}
	// Reject single-letter schemes like "c:" (Windows drive letter) that
	// url.Parse otherwise happily accepts.
	if len(u.Scheme) == 1 {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "ftp", "ftps", "smb", "nfs",
		"plugin", "rtsp", "rtmp", "rtmps", "mms", "webdav":
		return true
	}
	// Any other multi-char scheme (magnet, custom) — treat as URL too.
	return true
}
