package handler

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"

	"github.com/felipemarinho97/torrent-indexer/schema"
)

func loadRedeTorrentFixture(t *testing.T, name string) *goquery.Document {
	t.Helper()
	f, err := os.Open("testdata/rede/" + name)
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

func Test_extractLinksRedeTorrent(t *testing.T) {
	doc := loadRedeTorrentFixture(t, "search_matrix.html")
	got := extractLinksRedeTorrent(doc)
	want := []string{
		"https://redestorrents.com/arquivo-matrix-tri-audio-torrent-dublado-dual-audio-legendado-download/",
		"https://redestorrents.com/matrix-resurrections-torrent-dublado-dual-audio-legendado-download/",
		"https://redestorrents.com/matrix-resurrections-legendado-torrent-baixar-download/",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("extractLinksRedeTorrent() = %v, want %v", got, want)
	}
}

func Test_extractMovieJSONLDRedeTorrent(t *testing.T) {
	doc := loadRedeTorrentFixture(t, "detail_matrix.html")
	title, year, imdb := extractMovieJSONLDRedeTorrent(doc)
	if title != "Matrix" || year != "1999" || imdb != "" {
		t.Errorf("extractMovieJSONLDRedeTorrent() = (%q, %q, %q), want (Matrix, 1999, \"\")", title, year, imdb)
	}
}

func Test_extractAudioRedeTorrent(t *testing.T) {
	doc := loadRedeTorrentFixture(t, "detail_matrix.html")
	got := extractAudioRedeTorrent(doc)
	want := []schema.Audio{schema.AudioEnglish, schema.AudioPortuguese}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("extractAudioRedeTorrent() = %v, want %v", got, want)
	}
}

func Test_extractMagnetsRedeTorrent(t *testing.T) {
	doc := loadRedeTorrentFixture(t, "detail_matrix.html")
	links, sizes := extractMagnetsRedeTorrent(doc)
	if len(links) != 2 {
		t.Fatalf("expected 2 magnets, got %d: %v", len(links), links)
	}
	if !strings.Contains(links[0], "51ca3ba90f04bf50394e9eb7e448ceea214d5ad6") ||
		!strings.Contains(links[1], "8cc4fdc2c777d1464d9043397309f45859b9297b") {
		t.Errorf("unexpected magnets/order: %v", links)
	}
	// each size comes from its own download row, not from the page-wide spec card
	if sizes[links[0]] != "796 MB" || sizes[links[1]] != "2.03 GB" {
		t.Errorf("unexpected sizes: %v", sizes)
	}
}

func Test_extractMagnetsRedeTorrent_singleReleaseFallsBackToSpecCard(t *testing.T) {
	html := `<html><body>
<div class="download-row"><div class="download-name">DOWNLOAD TORRENT DUAL ÁUDIO 1080P</div><a href="magnet:?xt=urn:btih:8cc4fdc2c777d1464d9043397309f45859b9297b&dn=x">Download</a></div>
<div class="spec-text"><small>Tamanho do Arquivo</small><strong>1.81 GB</strong></div>
</body></html>`
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		t.Fatal(err)
	}
	links, sizes := extractMagnetsRedeTorrent(doc)
	if len(links) != 1 || sizes[links[0]] != "1.81 GB" {
		t.Errorf("got links=%v sizes=%v", links, sizes)
	}
}

func Test_formatBytesToSizeRedeTorrent(t *testing.T) {
	tests := map[string]string{
		"":           "",
		"abc":        "",
		"2048":       "2.00 KB",
		"834666496":  "796.00 MB",
		"2179695411": "2.03 GB",
	}
	for in, want := range tests {
		if got := formatBytesToSizeRedeTorrent(in); got != want {
			t.Errorf("formatBytesToSizeRedeTorrent(%q) = %q, want %q", in, got, want)
		}
	}
}

func Test_extractMovieJSONLDRedeTorrent_tvSeries(t *testing.T) {
	// series pages (e.g. Animatrix) emit "TVSeries" instead of "Movie"
	html := `<html><head><script type="application/ld+json">
{"@context":"https://schema.org","@type":"TVSeries","name":"Animatrix - A História Antes de Matrix","copyrightYear":2003}
</script></head><body></body></html>`
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		t.Fatal(err)
	}
	title, year, _ := extractMovieJSONLDRedeTorrent(doc)
	if title != "Animatrix - A História Antes de Matrix" || year != "2003" {
		t.Errorf("got (%q, %q)", title, year)
	}
}

func Test_extractYearRedeTorrent(t *testing.T) {
	tests := map[string]string{
		`<h1 class="movie-title">Animatrix Torrent (2003) Dual Áudio Download</h1>`:    "2003",
		`<head><meta property="og:title" content="Matrix Torrent (1999) Dual"></head>`: "1999",
		`<time itemprop="datePublished" datetime="1986-01-01">1986</time>`:             "1986",
		`<h1 class="movie-title">Sem ano</h1>`:                                         "",
	}
	for html, want := range tests {
		doc, err := goquery.NewDocumentFromReader(strings.NewReader("<html>" + html + "</html>"))
		if err != nil {
			t.Fatal(err)
		}
		if got := extractYearRedeTorrent(doc); got != want {
			t.Errorf("extractYearRedeTorrent(%q) = %q, want %q", html, got, want)
		}
	}
}
