package importer

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPivKeyURedirect_E2EBehavior simulates what the server does when a client
// plays a strm-sourced item: it 302-redirects to the URL from video_media.
// We verify the redirect Location header contains the strm URL verbatim and
// that Go's url machinery doesn't double-encode the Chinese/bracket chars.
func TestPivKeyURedirect_E2EBehavior(t *testing.T) {
	const strmContent = "https://strm.pivkeyu.com/redirect?path=/Media/Video/Movie/动画电影/A-安彦良和・板野一郎原画摄影集-2014-[tmdb=502419]/安彦良和・板野一郎原画摄影集.2014.mp4&pickcode=bdgcvu6389rm784qw"

	// 1. Parse the .strm file as the scanner would.
	s := ParseSTRMBytes([]byte(strmContent))
	if s.Primary != strmContent {
		t.Fatalf("parser corrupted URL:\n  want %q\n  got  %q", strmContent, s.Primary)
	}
	if !s.IsURL {
		t.Fatal("expected URL, got local")
	}

	// 2. Simulate Videos.Play redirecting to s.Primary.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, s.Primary, http.StatusPermanentRedirect)
	}))
	defer server.Close()

	// Don't follow redirects; inspect the Location header directly.
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(server.URL + "/videos/abc/play.mkv")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusPermanentRedirect {
		t.Fatalf("status = %d, want 308", resp.StatusCode)
	}

	loc := resp.Header.Get("Location")

	// Go's http.Redirect may percent-encode non-ASCII chars and brackets in the
	// Location header; clients then decode them on the way out. What matters
	// is that decoding loc yields the original URL.
	decoded, err := decodeLocation(loc)
	if err != nil {
		t.Fatalf("decode loc: %v", err)
	}
	if decoded != strmContent {
		t.Fatalf("Location does not round-trip:\n  want %q\n  got  %q\n  raw  %q", strmContent, decoded, loc)
	}

	t.Logf("Location header: %s", loc)
	t.Logf("Decoded:         %s", decoded)
}

// decodeLocation mimics how a real HTTP client decodes a Location header:
// it applies query-unescape to the percent-encoded portions.
func decodeLocation(loc string) (string, error) {
	// Split on '?' so we don't decode scheme / host unnecessarily.
	if i := strings.Index(loc, "?"); i >= 0 {
		pathPart, err := percentDecode(loc[:i])
		if err != nil {
			return "", err
		}
		queryPart, err := percentDecode(loc[i+1:])
		if err != nil {
			return "", err
		}
		return pathPart + "?" + queryPart, nil
	}
	return percentDecode(loc)
}

func percentDecode(s string) (string, error) {
	// url.PathUnescape handles the percent-encoded bytes correctly.
	// We use a local minimal version to avoid pulling net/url into stdlib twice.
	return unescape(s)
}

// unescape is a tiny helper that reverses %XX sequences.
func unescape(s string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			b, err := hexByte(s[i+1], s[i+2])
			if err != nil {
				return "", err
			}
			out.WriteByte(b)
			i += 2
			continue
		}
		out.WriteByte(s[i])
	}
	return out.String(), nil
}

func hexByte(a, b byte) (byte, error) {
	h := func(c byte) (byte, error) {
		switch {
		case c >= '0' && c <= '9':
			return c - '0', nil
		case c >= 'a' && c <= 'f':
			return c - 'a' + 10, nil
		case c >= 'A' && c <= 'F':
			return c - 'A' + 10, nil
		}
		return 0, &hexErr{c: c}
	}
	hi, err := h(a)
	if err != nil {
		return 0, err
	}
	lo, err := h(b)
	if err != nil {
		return 0, err
	}
	return hi<<4 | lo, nil
}

type hexErr struct{ c byte }

func (e *hexErr) Error() string { return "bad hex: " + string(e.c) }
