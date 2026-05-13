package importer

import (
	"io"
	"strings"
)

// lenientCharsetReader is the xml.Decoder CharsetReader hook. We only handle
// the common encodings that show up in NFO files. For anything else we pass
// raw bytes through — most NFOs are UTF-8 or ASCII, so this is almost always
// correct. Trans-coding GBK/Shift-JIS would require an external dep which we
// deliberately skip for now.
func lenientCharsetReader(charset string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
	}
	// Default: return the raw reader; decoder will do its best.
	return input, nil
}
