package importer

import (
	"strings"
	"testing"
	"time"
)

// ---------- STRM ----------

func TestParseSTRM_SingleURL(t *testing.T) {
	s := ParseSTRMBytes([]byte("https://cdn.example.com/movie.mkv\n"))
	if s.Primary != "https://cdn.example.com/movie.mkv" {
		t.Fatalf("primary = %q", s.Primary)
	}
	if !s.IsURL {
		t.Fatalf("expected URL")
	}
	if len(s.Targets) != 1 {
		t.Fatalf("targets = %v", s.Targets)
	}
}

func TestParseSTRM_SkipsCommentsAndBlankLines(t *testing.T) {
	body := "# alias\nhttps://primary.example.com/a.mkv\r\n\n// fallback\nhttps://fallback.example.com/a.mkv\n"
	s := ParseSTRMBytes([]byte(body))
	if s.Primary != "https://primary.example.com/a.mkv" {
		t.Fatalf("primary = %q", s.Primary)
	}
	if len(s.Targets) != 2 {
		t.Fatalf("targets = %v", s.Targets)
	}
	if s.Targets[1] != "https://fallback.example.com/a.mkv" {
		t.Fatalf("fallback = %q", s.Targets[1])
	}
}

func TestParseSTRM_BOMStripped(t *testing.T) {
	body := append([]byte{0xEF, 0xBB, 0xBF}, []byte("https://x.example.com/a.mkv")...)
	s := ParseSTRMBytes(body)
	if s.Primary != "https://x.example.com/a.mkv" {
		t.Fatalf("primary = %q", s.Primary)
	}
}

func TestParseSTRM_LocalAbsolutePath(t *testing.T) {
	s := ParseSTRMBytes([]byte("/mnt/media/movie.mkv\n"))
	if s.IsURL {
		t.Fatalf("expected local path")
	}
	if s.Primary != "/mnt/media/movie.mkv" {
		t.Fatalf("primary = %q", s.Primary)
	}
}

func TestParseSTRM_WindowsDriveNotScheme(t *testing.T) {
	// "c:/foo" should be parsed as a local path, not a URL.
	s := ParseSTRMBytes([]byte("c:/videos/movie.mkv\n"))
	if s.IsURL {
		t.Fatalf("windows drive should not be treated as URL")
	}
}

func TestParseSTRM_PluginURL(t *testing.T) {
	s := ParseSTRMBytes([]byte("plugin://plugin.video.example/play?id=1\n"))
	if !s.IsURL {
		t.Fatalf("plugin:// should be URL")
	}
}

// ---------- Filename ----------

func TestParseFilename_TitleYear(t *testing.T) {
	p := ParseFilename("The Wandering Earth II (2023).mkv")
	if p.Title != "The Wandering Earth II" {
		t.Fatalf("title = %q", p.Title)
	}
	if p.Year != 2023 {
		t.Fatalf("year = %d", p.Year)
	}
	if p.IsEpisode() {
		t.Fatalf("should not be episode")
	}
}

func TestParseFilename_SxxExx(t *testing.T) {
	p := ParseFilename("Game.of.Thrones.S01E02.Pilot.mkv")
	if p.Season != 1 || p.Episode != 2 {
		t.Fatalf("SxxExx = %+v", p)
	}
	if p.Title != "Game of Thrones" {
		t.Fatalf("title = %q", p.Title)
	}
}

func TestParseFilename_NxN(t *testing.T) {
	p := ParseFilename("Show.Name.1x05.Title.mp4")
	if p.Season != 1 || p.Episode != 5 {
		t.Fatalf("NxN = %+v", p)
	}
}

func TestParseFilename_JustE(t *testing.T) {
	p := ParseFilename("Show_E07_Title.mkv")
	if p.Episode != 7 {
		t.Fatalf("episode = %d", p.Episode)
	}
}

// TestParseFilename_ProviderTags covers Emby/Jellyfin folder conventions like
// "A-安彦良和・板野一郎原画摄影集-2014-[tmdb=502419]" and variants.
func TestParseFilename_ProviderTags(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantTmdb string
		wantImdb string
		wantTvdb string
	}{
		{"tmdb equals", "Foo (2014) [tmdb=502419].mkv", "502419", "", ""},
		{"tmdb dash", "Foo [tmdb-502419].mkv", "502419", "", ""},
		{"tmdbid alias", "Foo [tmdbid=502419].mkv", "502419", "", ""},
		{"imdb tag", "Foo (2014) [imdb=tt1234567].mkv", "", "tt1234567", ""},
		{"tvdb tag", "Show [tvdb=99999].mkv", "", "", "99999"},
		{"chinese with tmdb", "A-安彦良和・板野一郎原画摄影集-2014-[tmdb=502419]", "502419", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := ParseFilename(tc.in)
			if p.TMDBID != tc.wantTmdb {
				t.Errorf("tmdb = %q, want %q", p.TMDBID, tc.wantTmdb)
			}
			if p.IMDBID != tc.wantImdb {
				t.Errorf("imdb = %q, want %q", p.IMDBID, tc.wantImdb)
			}
			if p.TVDBID != tc.wantTvdb {
				t.Errorf("tvdb = %q, want %q", p.TVDBID, tc.wantTvdb)
			}
			// Provider tag must not bleed into the title.
			if strings.Contains(p.Title, "[") || strings.Contains(p.Title, "tmdb") {
				t.Errorf("provider tag leaked into title: %q", p.Title)
			}
		})
	}
}

func TestParseSeasonFolder(t *testing.T) {
	tests := map[string]int{
		"Season 1":   1,
		"season 02":  2,
		"Season_3":   3,
		"第一季":        1,
		"第 2 季":     2,
		"第10季":       10,
		"Specials":   0,
		"Not A Season": -1,
	}
	for input, want := range tests {
		got := ParseSeasonFolder(input)
		if got != want {
			t.Errorf("ParseSeasonFolder(%q) = %d, want %d", input, got, want)
		}
	}
}

// ---------- NFO ----------

const movieNFOSample = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<movie>
  <title>流浪地球 2</title>
  <originaltitle>The Wandering Earth II</originaltitle>
  <plot>太阳即将毁灭,人类面临流浪危机</plot>
  <year>2023</year>
  <premiered>2023-01-22</premiered>
  <runtime>173</runtime>
  <uniqueid type="tmdb" default="true">693134</uniqueid>
  <uniqueid type="imdb">tt15302324</uniqueid>
  <tmdbid>693134</tmdbid>
  <art>
    <poster>https://image.tmdb.org/t/p/w400/poster.jpg</poster>
    <fanart>https://image.tmdb.org/t/p/original/backdrop.jpg</fanart>
  </art>
  <genre>科幻</genre>
  <genre>动作</genre>
</movie>`

func TestParseNFO_Movie(t *testing.T) {
	n, err := ParseNFOBytes([]byte(movieNFOSample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n.Kind() != NFOKindMovie {
		t.Fatalf("kind = %s", n.Kind())
	}
	if n.Title != "流浪地球 2" {
		t.Fatalf("title = %q", n.Title)
	}
	if n.OriginalTitle != "The Wandering Earth II" {
		t.Fatalf("original = %q", n.OriginalTitle)
	}
	if n.Tmdb() != "693134" {
		t.Fatalf("tmdb = %q", n.Tmdb())
	}
	if n.Imdb() != "tt15302324" {
		t.Fatalf("imdb = %q", n.Imdb())
	}
	want := time.Date(2023, 1, 22, 0, 0, 0, 0, time.UTC)
	if !n.AirDate().Equal(want) {
		t.Fatalf("air = %s", n.AirDate())
	}
	if n.PosterURL() != "https://image.tmdb.org/t/p/w400/poster.jpg" {
		t.Fatalf("poster = %q", n.PosterURL())
	}
	if n.BackdropURL() != "https://image.tmdb.org/t/p/original/backdrop.jpg" {
		t.Fatalf("backdrop = %q", n.BackdropURL())
	}
	if n.Runtime != 173 {
		t.Fatalf("runtime = %d", n.Runtime)
	}
	if len(n.Genres) != 2 {
		t.Fatalf("genres = %v", n.Genres)
	}
}

const episodeNFOSample = `<?xml version="1.0" encoding="UTF-8"?>
<episodedetails>
  <title>Winter Is Coming</title>
  <season>1</season>
  <episode>1</episode>
  <aired>2011-04-17</aired>
  <plot>Lord Stark investigates a mystery.</plot>
  <thumb>https://image.tmdb.org/t/p/w400/s01e01.jpg</thumb>
</episodedetails>`

func TestParseNFO_Episode(t *testing.T) {
	n, err := ParseNFOBytes([]byte(episodeNFOSample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n.Kind() != NFOKindEpisode {
		t.Fatalf("kind = %s", n.Kind())
	}
	if n.Season != 1 || n.Episode != 1 {
		t.Fatalf("S/E = %d/%d", n.Season, n.Episode)
	}
	if n.Title != "Winter Is Coming" {
		t.Fatalf("title = %q", n.Title)
	}
}

func TestParseNFO_KodiThumbAspects(t *testing.T) {
	src := `<?xml version="1.0"?>
<tvshow>
  <title>Foo</title>
  <thumb aspect="poster">https://x/poster.jpg</thumb>
  <thumb aspect="landscape">https://x/landscape.jpg</thumb>
  <fanart>
    <thumb>https://x/fanart.jpg</thumb>
  </fanart>
</tvshow>`
	n, err := ParseNFOBytes([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if n.PosterURL() != "https://x/poster.jpg" {
		t.Fatalf("poster = %q", n.PosterURL())
	}
	if n.BackdropURL() != "https://x/fanart.jpg" {
		t.Fatalf("fanart = %q", n.BackdropURL())
	}
}

func TestParseNFO_MissingFieldsOk(t *testing.T) {
	n, err := ParseNFOBytes([]byte(`<movie><title>Just A Title</title></movie>`))
	if err != nil {
		t.Fatal(err)
	}
	if n.Title != "Just A Title" {
		t.Fatalf("title = %q", n.Title)
	}
	if n.PosterURL() != "" || n.BackdropURL() != "" {
		t.Fatalf("expected empty art")
	}
	if !n.AirDate().IsZero() {
		t.Fatalf("expected zero airdate")
	}
}
