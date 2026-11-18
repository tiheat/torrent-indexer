package handler

import (
	"os"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

func TestBuildBluDVRESTURL(t *testing.T) {
	got := buildBluDVRESTURL("https://bludvfilmes2.xyz/", "super mario", "2")
	want := "https://bludvfilmes2.xyz/wp-json/wp/v2/posts?_fields=id%2Cdate_gmt%2Clink%2Ctitle%2Ccontent&page=2&per_page=20&search=super+mario"
	if got != want {
		t.Errorf("buildBluDVRESTURL() = %q, want %q", got, want)
	}
}

func TestExtractBluDVSearchLinks(t *testing.T) {
	f, err := os.Open("testdata/bludv/search_matrix.html")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	doc, err := goquery.NewDocumentFromReader(f)
	if err != nil {
		t.Fatal(err)
	}
	links := extractBluDVSearchLinks(doc)
	if len(links) != 3 {
		t.Fatalf("expected 3 links, got %d: %v", len(links), links)
	}
	if !strings.HasPrefix(links[0], "https://bludvfilmes2.xyz/matrix-resurrections-") {
		t.Errorf("unexpected first link: %s", links[0])
	}
}

func TestParseBluDVRESTPosts(t *testing.T) {
	body, err := os.ReadFile("testdata/bludv/rest_search_matrix.html")
	if err != nil {
		t.Fatal(err)
	}
	posts, err := parseBluDVRESTPosts(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 2 {
		t.Fatalf("expected 2 posts, got %d", len(posts))
	}

	tests := []struct {
		title   string
		year    string
		magnets int
	}{
		{"Matrix Resurrections Torrent – WEB-DL 720p/1080p/4K Dual Áudio", "2021", 7},
		{"Trilogia Matrix Torrent – BluRay 720p e 1080p Dual Áudio (1999-2003)", "1999", 2},
	}
	for idx, tt := range tests {
		data, err := parseBluDVRESTPost(posts[idx])
		if err != nil {
			t.Fatal(err)
		}
		if data.Title != tt.title {
			t.Errorf("title = %q, want %q", data.Title, tt.title)
		}
		if data.Year != tt.year {
			t.Errorf("%s: year = %q, want %q", tt.title, data.Year, tt.year)
		}
		if len(data.MagnetLinks) != tt.magnets {
			t.Errorf("%s: magnets = %d, want %d", tt.title, len(data.MagnetLinks), tt.magnets)
		}
		for _, m := range data.MagnetLinks {
			if !strings.HasPrefix(m, "magnet:?xt=urn:btih:") || strings.Contains(m, "&#038;") || strings.Contains(m, "&amp;") {
				t.Errorf("%s: malformed magnet link %q", tt.title, m)
			}
		}
		if len(data.Audio) == 0 {
			t.Errorf("%s: expected audio to be detected", tt.title)
		}
		if data.Date.IsZero() {
			t.Errorf("%s: expected date to be parsed", tt.title)
		}
		if !strings.HasPrefix(data.Link, "https://bludvfilmes2.xyz/") {
			t.Errorf("%s: unexpected link %q", tt.title, data.Link)
		}
	}
}

func TestGetMagnetExactLength(t *testing.T) {
	if got := getMagnetExactLength("magnet:?xt=urn:btih:abc&dn=x&xl=2561627379"); got != "2.39 GB" {
		t.Errorf("got %q", got)
	}
	if got := getMagnetExactLength("magnet:?xt=urn:btih:abc&dn=x"); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestBluDVLinkSizes(t *testing.T) {
	body, err := os.ReadFile("testdata/bludv/rest_search_matrix.html")
	if err != nil {
		t.Fatal(err)
	}
	posts, err := parseBluDVRESTPosts(body)
	if err != nil {
		t.Fatal(err)
	}
	data, err := parseBluDVRESTPost(posts[0])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"3.21 GB", "7.89 GB", "19.03 GB", "2.82 GB", "4.29 GB", "9.35 GB", "19.32 GB"}
	for idx, m := range data.MagnetLinks {
		if got := data.LinkSizes[m]; got != want[idx] {
			t.Errorf("magnet %d: size = %q, want %q", idx, got, want[idx])
		}
	}
}
