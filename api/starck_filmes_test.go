package handler

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"

	"github.com/felipemarinho97/torrent-indexer/schema"
)

func loadStarckFixture(t *testing.T, name string) *goquery.Document {
	t.Helper()
	f, err := os.Open("testdata/starck/" + name)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	doc, err := goquery.NewDocumentFromReader(f)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return doc
}

func Test_parseStarckFilmesSearchLinks(t *testing.T) {
	links := parseStarckFilmesSearchLinks(loadStarckFixture(t, "search.html"))
	want := []string{
		"https://starckfilmes-v30.com/catalog/matrix-resurrections-2021/",
		"https://starckfilmes-v30.com/catalog/animatrix-2003/",
		"https://starckfilmes-v30.com/catalog/trilogia-matrix-1999-2003/",
	}
	if len(links) != len(want) {
		t.Fatalf("got %d links (%v), want %d", len(links), links, len(want))
	}
	for i := range want {
		if links[i] != want[i] {
			t.Errorf("link[%d] = %q, want %q", i, links[i], want[i])
		}
	}
}

func Test_parseStarckFilmesDetail_movie(t *testing.T) {
	d := parseStarckFilmesDetail(loadStarckFixture(t, "detail_movie.html"))
	if d.Title != "Matrix: Resurrections" {
		t.Errorf("title = %q", d.Title)
	}
	if d.Year != "2021" {
		t.Errorf("year = %q", d.Year)
	}
	if len(d.Audio) != 2 || d.Audio[0] != schema.AudioPortuguese {
		t.Errorf("audio = %v", d.Audio)
	}
	if len(d.Downloads) != 2 {
		t.Fatalf("got %d downloads, want 2", len(d.Downloads))
	}
	wantSizes := []string{"2.83 GB", "19.03 GB"}
	wantDN := []string{"The.Matrix.Resurrections.2021.1080p", "Matrix%20Resurrections%202021%20WEB-DL%202160p"}
	for i, dl := range d.Downloads {
		if !strings.HasPrefix(dl.Magnet, "magnet:?xt=urn:btih:") {
			t.Errorf("download[%d] magnet = %q", i, dl.Magnet)
		}
		if !strings.Contains(dl.Magnet, wantDN[i]) {
			t.Errorf("download[%d] magnet %q does not contain %q", i, dl.Magnet, wantDN[i])
		}
		if dl.Size != wantSizes[i] {
			t.Errorf("download[%d] size = %q, want %q", i, dl.Size, wantSizes[i])
		}
	}
}

func Test_parseStarckFilmesDetail_series(t *testing.T) {
	d := parseStarckFilmesDetail(loadStarckFixture(t, "detail_series.html"))
	if d.Title != "Lanternas 1ª Temporada" || d.Year != "2026" {
		t.Errorf("title/year = %q/%q", d.Title, d.Year)
	}
	if len(d.Downloads) != 8 {
		t.Fatalf("got %d downloads, want 8", len(d.Downloads))
	}
	for i, dl := range d.Downloads {
		if !strings.HasPrefix(dl.Magnet, "magnet:?xt=urn:btih:") {
			t.Errorf("download[%d] magnet = %q", i, dl.Magnet)
		}
	}
}

func Test_decodeStarckFilmesXid_invalid(t *testing.T) {
	if _, err := decodeStarckFilmesXid("https://filmedl.com/?foo=bar"); err == nil {
		t.Error("expected error for missing xid")
	}
	if _, err := decodeStarckFilmesXid("https://filmedl.com/?xid=notbase64"); err == nil {
		t.Error("expected error for garbage xid")
	}
}

func Test_getStarckFilmesPublishedDate(t *testing.T) {
	tests := map[string]time.Time{
		"https://starckfilmes-v30.com/catalog/a-odisseia-2026-08-11-2026/":        time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC),
		"https://www.starckfilmes.fans/catalog/jogos-de-seducao-2025-18-07-2025/": time.Date(2025, 7, 18, 0, 0, 0, 0, time.UTC),
		"https://starckfilmes-v30.com/catalog/matrix-resurrections-2021/":         {},
	}
	for link, want := range tests {
		if got := getStarckFilmesPublishedDate(link); !got.Equal(want) {
			t.Errorf("%s: got %v, want %v", link, got, want)
		}
	}
}
