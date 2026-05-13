package importer

import (
	"net/url"
	"testing"
)

// TestParseSTRM_PivKeyURedirect verifies that Emotion correctly handles the
// real-world strm redirect URLs served by strm.pivkeyu.com.
//
// Properties we check:
//  1. The URL is classified as a URL (not a local path).
//  2. The Primary field preserves the entire URL byte-for-byte, including
//     UTF-8 Chinese path components, square brackets, and query params.
//  3. url.Parse can extract scheme / host / query so 302 redirects work.
//  4. BOM-prefixed content and multi-line STRM files are handled correctly.
func TestParseSTRM_PivKeyURedirect(t *testing.T) {
	const raw = "https://strm.pivkeyu.com/redirect?path=/Media/Video/Movie/动画电影/A-安彦良和・板野一郎原画摄影集-2014-[tmdb=502419]/安彦良和・板野一郎原画摄影集.2014.mp4&pickcode=bdgcvu6389rm784qw"

	t.Run("single line", func(t *testing.T) {
		s := ParseSTRMBytes([]byte(raw))
		if !s.IsURL {
			t.Fatalf("expected IsURL=true, got false")
		}
		if s.Primary != raw {
			t.Fatalf("Primary mangled:\n  want %q\n  got  %q", raw, s.Primary)
		}
		if len(s.Targets) != 1 {
			t.Fatalf("Targets len = %d, want 1", len(s.Targets))
		}
		// The URL must be parseable so http.Redirect can use it.
		u, err := url.Parse(s.Primary)
		if err != nil {
			t.Fatalf("url.Parse failed: %v", err)
		}
		if u.Scheme != "https" {
			t.Fatalf("scheme = %q, want https", u.Scheme)
		}
		if u.Host != "strm.pivkeyu.com" {
			t.Fatalf("host = %q", u.Host)
		}
		if u.Path != "/redirect" {
			t.Fatalf("path = %q, want /redirect", u.Path)
		}
		// Query params preserved exactly (Chinese + brackets + ampersand).
		if u.Query().Get("pickcode") != "bdgcvu6389rm784qw" {
			t.Fatalf("pickcode param lost: %q", u.Query().Get("pickcode"))
		}
		gotPath := u.Query().Get("path")
		wantPath := "/Media/Video/Movie/动画电影/A-安彦良和・板野一郎原画摄影集-2014-[tmdb=502419]/安彦良和・板野一郎原画摄影集.2014.mp4"
		if gotPath != wantPath {
			t.Fatalf("path param mangled:\n  want %q\n  got  %q", wantPath, gotPath)
		}
	})

	t.Run("with trailing newline", func(t *testing.T) {
		s := ParseSTRMBytes([]byte(raw + "\n"))
		if s.Primary != raw {
			t.Fatalf("trailing newline broke URL: %q", s.Primary)
		}
	})

	t.Run("with CRLF", func(t *testing.T) {
		s := ParseSTRMBytes([]byte(raw + "\r\n"))
		if s.Primary != raw {
			t.Fatalf("CRLF broke URL: %q", s.Primary)
		}
	})

	t.Run("with UTF-8 BOM", func(t *testing.T) {
		body := append([]byte{0xEF, 0xBB, 0xBF}, []byte(raw)...)
		s := ParseSTRMBytes(body)
		if s.Primary != raw {
			t.Fatalf("BOM stripping broke URL:\n  want %q\n  got  %q", raw, s.Primary)
		}
	})

	t.Run("multi line with comment and fallback", func(t *testing.T) {
		body := "# 主线路\n" + raw + "\n# 备份\nhttps://fallback.example.com/a.mp4\n"
		s := ParseSTRMBytes([]byte(body))
		if s.Primary != raw {
			t.Fatalf("multi-line primary wrong: %q", s.Primary)
		}
		if len(s.Targets) != 2 {
			t.Fatalf("Targets len = %d, want 2", len(s.Targets))
		}
		if s.Targets[1] != "https://fallback.example.com/a.mp4" {
			t.Fatalf("fallback wrong: %q", s.Targets[1])
		}
	})
}
