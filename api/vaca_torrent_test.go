package handler

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"

	"github.com/felipemarinho97/torrent-indexer/schema"
)

func loadVacaTorrentFixture(t *testing.T, name string) *goquery.Document {
	t.Helper()
	f, err := os.Open("testdata/vaca/" + name)
	if err != nil {
		t.Fatalf("failed to open fixture: %v", err)
	}
	defer f.Close()
	doc, err := goquery.NewDocumentFromReader(f)
	if err != nil {
		t.Fatalf("failed to parse fixture: %v", err)
	}
	return doc
}

func Test_parseVacaTorrentAjaxResponse(t *testing.T) {
	body, err := os.ReadFile("testdata/vaca/search_matrix.json")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parseVacaTorrentAjaxResponse(200, "", body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := extractLinksVacaTorrent(doc)
	if len(got) != 8 {
		t.Fatalf("expected 8 links, got %d: %v", len(got), got)
	}
	for _, l := range got {
		if !strings.Contains(l, "matrix") {
			t.Errorf("unexpected link %q", l)
		}
	}
	if got[0] != "https://vaqueirofilmes.com/pt/movie/animatrix/" {
		t.Errorf("first link = %q", got[0])
	}

	// the old domain returned HTML / redirects instead of JSON
	if _, err := parseVacaTorrentAjaxResponse(200, "", []byte("<html></html>")); err == nil {
		t.Error("expected error for HTML body")
	}
	if _, err := parseVacaTorrentAjaxResponse(301, "https://example.com/", nil); err == nil || !strings.Contains(err.Error(), "INDEXER_VACA_TORRENT_URL") {
		t.Errorf("expected redirect error, got %v", err)
	}
}

func Test_parseVacaTorrentPage_legacyPost(t *testing.T) {
	doc := loadVacaTorrentFixture(t, "legacy_post.html")
	p := parseVacaTorrentPage(doc, "https://vaqueirofilmes.com/super-mario-bros-o-filme-torrent-2023-web-dl-720p-1080p-4k-assistir-online-dual-udio/")

	if p.Title != "Super Mario Bros.: O Filme" {
		t.Errorf("Title = %q", p.Title)
	}
	if p.Year != "2023" {
		t.Errorf("Year = %q", p.Year)
	}
	if want := []schema.Audio{schema.AudioPortuguese, schema.AudioEnglish}; !reflect.DeepEqual(p.Audio, want) {
		t.Errorf("Audio = %v, want %v", p.Audio, want)
	}
	if want := []string{"2.17 GB", "5.99 GB"}; !reflect.DeepEqual(p.Sizes, want) {
		t.Errorf("Sizes = %v, want %v", p.Sizes, want)
	}
	if len(p.Links) != 8 {
		t.Fatalf("expected 8 links, got %d", len(p.Links))
	}
	first := p.Links[0]
	if !strings.Contains(first.URL, "/enc/go.php?id=") {
		t.Errorf("unexpected link URL %q", first.URL)
	}
	if _, quality, size := parseVacaTorrentLinkLabel(first.Label); size != "2.17 GB" || quality != "1080p" {
		t.Errorf("label %q parsed to quality=%q size=%q", first.Label, quality, size)
	}
	if p.Season != "" || p.LinksPage != "" || p.SeasonPage != "" {
		t.Errorf("unexpected season/links page: %+v", p)
	}
}

func Test_parseVacaTorrentPage_moviePage(t *testing.T) {
	doc := loadVacaTorrentFixture(t, "movie_page.html")
	p := parseVacaTorrentPage(doc, "https://vaqueirofilmes.com/pt/movie/matrix/")
	if p.Title != "Matrix" || p.Year != "1999" {
		t.Errorf("Title/Year = %q/%q", p.Title, p.Year)
	}
	if p.IMDB != "https://www.imdb.com/title/tt0133093/" {
		t.Errorf("IMDB = %q", p.IMDB)
	}
	if len(p.Links) != 0 {
		t.Errorf("expected no links, got %v", p.Links)
	}
	if p.LinksPage != "https://vaqueirofilmes.com/movie-links/40394/" {
		t.Errorf("LinksPage = %q", p.LinksPage)
	}
}

func Test_extractDownloadLinksVacaTorrent_movieLinks(t *testing.T) {
	doc := loadVacaTorrentFixture(t, "movie_links.html")
	links := extractDownloadLinksVacaTorrent(doc)
	if len(links) != 2 {
		t.Fatalf("expected 2 links, got %d: %v", len(links), links)
	}
	audio, quality, size := parseVacaTorrentLinkLabel(links[1].Label)
	if want := []schema.Audio{schema.AudioPortuguese, schema.AudioEnglish}; !reflect.DeepEqual(audio, want) {
		t.Errorf("audio = %v, want %v", audio, want)
	}
	if quality != "2160p x265 HDR" || size != "10.45 GB" {
		t.Errorf("quality=%q size=%q (label %q)", quality, size, links[1].Label)
	}
}

func Test_parseVacaTorrentPage_tvShowAndSeason(t *testing.T) {
	show := parseVacaTorrentPage(loadVacaTorrentFixture(t, "tv_show.html"), "https://vaqueirofilmes.com/pt/tv-shows/a-ilha-do-tesouro/")
	if show.Title != "A Ilha do Tesouro" || show.Year != "2026" {
		t.Errorf("Title/Year = %q/%q", show.Title, show.Year)
	}
	if show.SeasonPage != "https://vaqueirofilmes.com/pt/tv/a-ilha-do-tesouro/season/temporada-1/" {
		t.Errorf("SeasonPage = %q", show.SeasonPage)
	}

	season := parseVacaTorrentPage(loadVacaTorrentFixture(t, "season_page.html"), show.SeasonPage)
	if season.Season != "1" {
		t.Errorf("Season = %q", season.Season)
	}
	if len(season.Links) != 2 {
		t.Fatalf("expected 2 links, got %d", len(season.Links))
	}
	if season.Links[0].Episode != "01" || season.Links[1].Episode != "02" {
		t.Errorf("episodes = %q, %q", season.Links[0].Episode, season.Links[1].Episode)
	}

	show.Season = season.Season
	_, quality, _ := parseVacaTorrentLinkLabel(season.Links[1].Label)
	audio := []schema.Audio{schema.AudioPortuguese, schema.AudioEnglish}
	if got := buildVacaTorrentReleaseTitle(show, season.Links[1], quality, audio); got != "A Ilha do Tesouro S01E02 1080p DUAL" {
		t.Errorf("release title = %q", got)
	}
	if got := processVacaTorrentTitle(show.Title, []schema.Audio{schema.AudioPortuguese}, show.Season); got != "A Ilha do Tesouro S01 - 1ª Temporada (brazilian)" {
		t.Errorf("original title = %q", got)
	}
}

func Test_extractMagnetFromVacaTorrentReceber(t *testing.T) {
	got, err := extractMagnetFromVacaTorrentReceber(loadVacaTorrentFixture(t, "receber.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "magnet:?xt=urn:btih:172863ab4080c8c2538da526b2e1d37c4f4d129f&dn=Matrix+Reloaded") {
		t.Errorf("magnet = %q", got)
	}
	if strings.Contains(got, "&amp;") {
		t.Errorf("magnet was not unescaped: %q", got)
	}

	doc, _ := goquery.NewDocumentFromReader(strings.NewReader("<html><body></body></html>"))
	if _, err := extractMagnetFromVacaTorrentReceber(doc); err == nil {
		t.Error("expected error without data-link")
	}
}

func Test_getVacaTorrentMagnetLength(t *testing.T) {
	if got := getVacaTorrentMagnetLength("magnet:?xt=urn:btih:abc&xl=2040408708&dn=x"); got == "" {
		t.Error("expected size from xl")
	}
	if got := getVacaTorrentMagnetLength("magnet:?xt=urn:btih:abc"); got != "" {
		t.Errorf("expected empty size, got %q", got)
	}
}

func Test_fixVacaTorrentMagnetTrackers(t *testing.T) {
	in := "magnet:?xt=urn:btih:6f89c9dc2db8410d719b9eb14773467c72241268&tr=udptracker.opentrackr.org1337announce&tr=httpstrackers.mlsub.net443announce&tr=udp%3A%2F%2Fopen.stealth.si%3A80%2Fannounce&tr=udpu4.trakx.crim.ist1337anunciar"
	want := "magnet:?xt=urn:btih:6f89c9dc2db8410d719b9eb14773467c72241268" +
		"&tr=udp%3A%2F%2Ftracker.opentrackr.org%3A1337%2Fannounce" +
		"&tr=https%3A%2F%2Ftrackers.mlsub.net%3A443%2Fannounce" +
		"&tr=udp%3A%2F%2Fopen.stealth.si%3A80%2Fannounce" +
		"&tr=udp%3A%2F%2Fu4.trakx.crim.ist%3A1337%2Fannounce"
	if got := fixVacaTorrentMagnetTrackers(in); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	ok := "magnet:?xt=urn:btih:abc&dn=x&tr=udp%3A%2F%2Ftracker.opentrackr.org%3A1337%2Fannounce"
	if got := fixVacaTorrentMagnetTrackers(ok); got != ok {
		t.Errorf("valid magnet changed: %s", got)
	}
}
