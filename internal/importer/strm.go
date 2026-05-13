package importer

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// STRM represents the parsed content of a .strm file.
//
// STRM files are simple text pointers Emby/Kodi treat as virtual videos.
// In the wild they come in several flavors:
//
//  1. Single URL (most common):
//       https://cdn.example.com/movie.mkv
//
//  2. URL with query params / signed URLs (cloud drives):
//       https://pan.example.com/redirect?fid=123&sign=abc
//
//  3. plugin:// URLs (Kodi-only); we treat as opaque URL.
//
//  4. Local absolute or relative paths:
//       /mnt/media/movies/foo.mkv
//       ../downloads/bar.mp4
//
//  5. Multi-line with comments (primary first, fallback after):
//       # preferred source
//       https://primary.example.com/movie.mkv
//       https://fallback.example.com/movie.mkv
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
