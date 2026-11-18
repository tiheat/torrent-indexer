package handler

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/felipemarinho97/torrent-indexer/logging"
	goscrape "github.com/felipemarinho97/torrent-indexer/scrape"
)

type searchQueryKey struct{}

// withSearchQuery stores the search query in the context when the request asks
// for filtered results, so scrapers can skip work for torrents that will be
// filtered out anyway.
func withSearchQuery(ctx context.Context, r *http.Request) context.Context {
	q := r.URL.Query().Get("q")
	if q == "" || r.URL.Query().Get("filter_results") == "" {
		return ctx
	}
	return context.WithValue(ctx, searchQueryKey{}, q)
}

func searchQueryFromContext(ctx context.Context) string {
	q, _ := ctx.Value(searchQueryKey{}).(string)
	return q
}

// scrapePeers returns the leechers and seeders of a torrent. Trackers are only
// queried when the torrent matches the filtered search query: a single post can
// hold hundreds of unrelated magnets (e.g. "listão" posts), and scraping all of
// them makes the request take minutes.
func scrapePeers(ctx context.Context, i *Indexer, infoHash string, trackers []string, titles ...string) (int, int) {
	if q := searchQueryFromContext(ctx); q != "" && !matchesQuery(q, titles...) {
		return 0, 0
	}

	peer, seed, err := goscrape.GetLeechsAndSeeds(ctx, i.redis, i.metrics, infoHash, trackers)
	if err != nil {
		logging.Error().Err(err).Str("info_hash", infoHash).Msg("Failed to get leechers and seeders")
	}
	return peer, seed
}

var accentReplacer = strings.NewReplacer(
	"á", "a", "à", "a", "â", "a", "ã", "a", "ä", "a",
	"é", "e", "è", "e", "ê", "e", "ë", "e",
	"í", "i", "ì", "i", "î", "i", "ï", "i",
	"ó", "o", "ò", "o", "ô", "o", "õ", "o", "ö", "o",
	"ú", "u", "ù", "u", "û", "u", "ü", "u",
	"ç", "c", "ñ", "n",
)

var nonAlphanumeric = regexp.MustCompile(`[^a-z0-9]+`)

// words of a query that don't identify a title
var queryStopwords = map[string]bool{
	"the": true, "an": true, "of": true, "and": true,
	"os": true, "as": true, "de": true, "do": true, "da": true, "dos": true,
	"das": true, "em": true, "no": true, "na": true, "um": true, "uma": true,
}

// season/episode markers and years are left to the *arr apps: release names
// often omit the year, and Brazilian posts write seasons as "1ª Temporada".
var ignoredQueryToken = regexp.MustCompile(`^(s\d{1,2}(e\d{1,3})?|e\d{1,3}|(19|20)\d{2})$`)

func normalizeWords(s string) []string {
	s = accentReplacer.Replace(strings.ToLower(s))
	return strings.Fields(nonAlphanumeric.ReplaceAllString(s, " "))
}

func queryTokens(q string) []string {
	var tokens []string
	for _, w := range normalizeWords(q) {
		isDigit := w[0] >= '0' && w[0] <= '9'
		if (len(w) < 2 && !isDigit) || queryStopwords[w] || ignoredQueryToken.MatchString(w) {
			continue
		}
		tokens = append(tokens, w)
	}
	return tokens
}

// matchesQuery reports whether every significant word of the query appears as a
// word in at least one of the given titles.
func matchesQuery(q string, titles ...string) bool {
	tokens := queryTokens(q)
	if len(tokens) == 0 {
		return true
	}

	words := map[string]bool{}
	for _, t := range titles {
		for _, w := range normalizeWords(t) {
			words[w] = true
		}
	}

	for _, token := range tokens {
		if !words[token] {
			return false
		}
	}
	return true
}
