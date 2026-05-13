package importer

import (
	"os"
	"path/filepath"
	"testing"
)

// TestScan_EndToEnd builds a representative on-disk layout and verifies the
// scanner groups files by directory and classifies them correctly.
func TestScan_EndToEnd(t *testing.T) {
	root := t.TempDir()

	layout := map[string]string{
		// A movie with an NFO and a poster sidecar.
		"Movies/Wandering Earth 2 (2023)/wandering-earth-2.mkv":    "fake",
		"Movies/Wandering Earth 2 (2023)/wandering-earth-2.nfo":    movieNFOSample,
		"Movies/Wandering Earth 2 (2023)/poster.jpg":               "img",
		"Movies/Wandering Earth 2 (2023)/wandering-earth-2.zh.srt": "sub",

		// A TV show with a tvshow.nfo and two episodes in "Season 1".
		"Shows/Game of Thrones/tvshow.nfo":              tvshowNFOSample,
		"Shows/Game of Thrones/Season 1/got.s01e01.mkv": "fake",
		"Shows/Game of Thrones/Season 1/got.s01e01.nfo": episodeNFOSample,
		"Shows/Game of Thrones/Season 1/got.s01e02.mkv": "fake",

		// A STRM-based movie.
		"Movies/Cloud Movie/cloud-movie.strm": "https://cdn.example.com/cloud-movie.mkv\n",
	}
	for rel, content := range layout {
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	dirs, err := Scan(ScanOptions{Root: root})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	// Expect 4 directories of interest.
	if got := len(dirs); got < 4 {
		t.Fatalf("expected >=4 dirs, got %d: %v", got, dirs)
	}

	// Movie dir: should contain 1 media, 1 NFO, 1 image, 1 sub.
	movieDir := filepath.Join(root, "Movies", "Wandering Earth 2 (2023)")
	if b, ok := dirs[movieDir]; !ok {
		t.Fatalf("missing %s", movieDir)
	} else {
		if len(b.Media) != 1 || len(b.NFOs) != 1 || len(b.Images) != 1 || len(b.Subtitles) != 1 {
			t.Fatalf("movie bucket = %+v", b)
		}
	}

	// Show root with tvshow.nfo.
	showDir := filepath.Join(root, "Shows", "Game of Thrones")
	if b, ok := dirs[showDir]; !ok {
		t.Fatalf("missing %s", showDir)
	} else if len(b.NFOs) != 1 {
		t.Fatalf("show NFOs = %v", b.NFOs)
	}

	// Season dir.
	seasonDir := filepath.Join(showDir, "Season 1")
	if b, ok := dirs[seasonDir]; !ok {
		t.Fatalf("missing %s", seasonDir)
	} else if len(b.Media) != 2 {
		t.Fatalf("season media = %v", b.Media)
	}

	// STRM dir.
	strmDir := filepath.Join(root, "Movies", "Cloud Movie")
	if b, ok := dirs[strmDir]; !ok {
		t.Fatalf("missing %s", strmDir)
	} else if len(b.Media) != 1 {
		t.Fatalf("strm media = %v", b.Media)
	}
}

const tvshowNFOSample = `<?xml version="1.0"?>
<tvshow>
  <title>Game of Thrones</title>
  <year>2011</year>
  <uniqueid type="tmdb">1399</uniqueid>
  <plot>Nine noble families fight for control of the Seven Kingdoms.</plot>
  <art>
    <poster>https://image.tmdb.org/t/p/w400/got-poster.jpg</poster>
  </art>
</tvshow>`
