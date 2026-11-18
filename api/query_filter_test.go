package handler

import (
	"context"
	"net/http/httptest"
	"testing"
)

func Test_matchesQuery(t *testing.T) {
	tests := []struct {
		name   string
		q      string
		titles []string
		want   bool
	}{
		{
			name:   "should match release with every query word",
			q:      "matrix revolutions",
			titles: []string{"Matrix Revolutions (2003) Bluray 1080p Dual Audio", "Matrix 3 Revolutions"},
			want:   true,
		},
		{
			name:   "should match words split by dots",
			q:      "matrix revolutions",
			titles: []string{"The.Matrix.Revolutions.2003.1080p.BluRay.x264", ""},
			want:   true,
		},
		{
			name:   "should not match unrelated magnet from a list post",
			q:      "matrix revolutions",
			titles: []string{"The.Raid.Redemption.2011.BDRip.XviD-MeRCuRY.avi", "Listão de Filmes Dublado Ação Terror Aventura"},
			want:   false,
		},
		{
			name:   "should not match when a query word is missing",
			q:      "matrix revolutions",
			titles: []string{"Matrix 2 Reloaded Dublado Pt Br", "Listão de Filmes Dublado"},
			want:   false,
		},
		{
			name:   "should match any of the titles",
			q:      "matrix revolutions",
			titles: []string{"Matrix 3 Dublado", "Matrix Revolutions"},
			want:   true,
		},
		{
			name:   "should ignore stopwords, year and case",
			q:      "The Matrix Revolutions 2003",
			titles: []string{"MATRIX REVOLUTIONS 1080p"},
			want:   true,
		},
		{
			name:   "should ignore accents",
			q:      "heranca em jogo",
			titles: []string{"Herança em Jogo S01 WEB-DL"},
			want:   true,
		},
		{
			name:   "should ignore season and episode markers",
			q:      "the good doctor s03e05",
			titles: []string{"The Good Doctor 3ª Temporada Completa"},
			want:   true,
		},
		{
			name:   "should require numbers that are part of the title",
			q:      "toy story 3",
			titles: []string{"Toy Story 2 1999 1080p"},
			want:   false,
		},
		{
			name:   "should match everything when the query has no significant word",
			q:      "the 2003",
			titles: []string{"Anything"},
			want:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesQuery(tt.q, tt.titles...); got != tt.want {
				t.Errorf("matchesQuery(%q, %q) = %v, want %v", tt.q, tt.titles, got, tt.want)
			}
		})
	}
}

func Test_withSearchQuery(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{name: "should keep query when filtering results", url: "/indexers/comando_torrents?filter_results=true&q=matrix", want: "matrix"},
		{name: "should ignore query without filter_results", url: "/indexers/comando_torrents?q=matrix", want: ""},
		{name: "should ignore filter_results without query", url: "/indexers/comando_torrents?filter_results=true", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", tt.url, nil)
			if got := searchQueryFromContext(withSearchQuery(context.Background(), r)); got != tt.want {
				t.Errorf("searchQueryFromContext() = %q, want %q", got, tt.want)
			}
		})
	}
}
