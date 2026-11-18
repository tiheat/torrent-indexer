package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"github.com/felipemarinho97/torrent-indexer/logging"
	"github.com/felipemarinho97/torrent-indexer/magnet"
	"github.com/felipemarinho97/torrent-indexer/schema"
	goscrape "github.com/felipemarinho97/torrent-indexer/scrape"
	"github.com/felipemarinho97/torrent-indexer/utils"
)

var starck_filmes = IndexerMeta{
	Label:       "starck_filmes",
	URL:         utils.GetIndexerURLFromEnv("INDEXER_STARCK_FILMES_URL", "https://starckfilmes-v30.com/"),
	SearchURL:   "?s=",
	PagePattern: "page/%s",
}

func (i *Indexer) HandlerStarckFilmesIndexer(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	metadata := starck_filmes

	defer func() {
		i.metrics.IndexerDuration.WithLabelValues(metadata.Label).Observe(time.Since(start).Seconds())
		i.metrics.IndexerRequests.WithLabelValues(metadata.Label).Inc()
	}()

	ctx := r.Context()
	// supported query params: q, page, filter_results
	q := r.URL.Query().Get("q")
	page := r.URL.Query().Get("page")

	// URL encode query param
	q = url.QueryEscape(q)
	url := metadata.URL
	if q != "" {
		url = fmt.Sprintf("%s%s%s", url, metadata.SearchURL, q)
	} else if page != "" {
		url = fmt.Sprintf(fmt.Sprintf("%s%s", url, metadata.PagePattern), page)
	}

	// if search is empty, redirect to page 1
	if q == "" {
		url = fmt.Sprintf(fmt.Sprintf("%s%s", url, metadata.PagePattern), "1")
	}

	logging.InfoWithRequest(r).Str("target_url", url).Msg("Processing indexer request")
	resp, err := i.requester.GetDocument(ctx, url)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		err = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		if err != nil {
			logging.ErrorWithRequest(r).Err(err).Msg("Failed to encode error response")
		}
		i.metrics.IndexerErrors.WithLabelValues(metadata.Label).Inc()
		return
	}
	defer resp.Close()

	doc, err := goquery.NewDocumentFromReader(resp)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		err = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		if err != nil {
			logging.ErrorWithRequest(r).Err(err).Msg("Failed to encode error response")
		}

		i.metrics.IndexerErrors.WithLabelValues(metadata.Label).Inc()
		return
	}

	links := parseStarckFilmesSearchLinks(doc)

	// if no links were indexed, expire the document in cache
	if len(links) == 0 {
		_ = i.requester.ExpireDocument(ctx, url)
		logging.DebugWithRequest(r).Str("url", url).Msg("No links found, expiring cache")
	}

	// extract each torrent link
	indexedTorrents := utils.ParallelFlatMap(links, func(link string) ([]schema.IndexedTorrent, error) {
		return getTorrentStarckFilmes(ctx, i, link, url)
	})

	// Apply post-processors
	postProcessedTorrents := indexedTorrents
	for _, processor := range i.postProcessors {
		postProcessedTorrents = processor(i, r, postProcessedTorrents)
	}

	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(Response{
		Results:      postProcessedTorrents,
		Count:        len(postProcessedTorrents),
		IndexedCount: len(indexedTorrents),
	})
	if err != nil {
		logging.Error().Err(err).Msg("Failed to encode response")
	}
}

func getTorrentStarckFilmes(ctx context.Context, i *Indexer, link, referer string) ([]schema.IndexedTorrent, error) {
	var indexedTorrents []schema.IndexedTorrent
	doc, err := getDocument(ctx, i, link, referer)
	if err != nil {
		return nil, err
	}

	date := getStarckFilmesPublishedDate(link)

	detail := parseStarckFilmesDetail(doc)
	title := detail.Title
	year := detail.Year
	audio := detail.Audio
	size := detail.Sizes

	magnetLinks := make([]string, 0, len(detail.Downloads))
	for _, d := range detail.Downloads {
		magnetLinks = append(magnetLinks, d.Magnet)
	}

	// TODO: find any link from imdb
	imdbLink := ""

	var chanIndexedTorrent = make(chan schema.IndexedTorrent)

	// for each magnet link, create a new indexed torrent
	for it, magnetLink := range magnetLinks {
		it := it
		go func(it int, magnetLink string) {
			magnet, err := magnet.ParseMagnetUri(magnetLink)
			if err != nil {
				logging.Error().Err(err).Str("magnet_link", magnetLink).Msg("Failed to parse magnet URI")
			}
			releaseTitle := strings.TrimSpace(magnet.DisplayName)
			// url decode the title
			releaseTitle, err = url.QueryUnescape(releaseTitle)
			if err != nil {
				logging.Error().Err(err).Str("title", releaseTitle).Msg("Failed to URL decode title")
				releaseTitle = strings.TrimSpace(magnet.DisplayName)
			}
			infoHash := magnet.InfoHash.String()
			trackers := magnet.Trackers
			for i, tracker := range trackers {
				unescapedTracker, err := url.QueryUnescape(tracker)
				if err != nil {
					logging.Error().Err(err).Str("tracker", tracker).Msg("Failed to URL decode tracker")
				}
				trackers[i] = strings.TrimSpace(unescapedTracker)
			}
			magnetAudio := getAudioFromTitle(releaseTitle, audio)

			peer, seed, err := goscrape.GetLeechsAndSeeds(ctx, i.redis, i.metrics, infoHash, trackers)
			if err != nil {
				logging.Error().Err(err).Str("info_hash", infoHash).Msg("Failed to get leechers and seeders")
			}

			title := processTitle(title, magnetAudio)

			// if the number of sizes is equal to the number of magnets, then assign the size to each indexed torrent in order
			mySize := detail.Downloads[it].Size
			if mySize == "" && len(size) == len(magnetLinks) {
				mySize = size[it]
			}
			if mySize == "" {
				go func() {
					_, _ = i.magnetMetadataAPI.FetchMetadata(ctx, magnetLink)
				}()
			}

			ixt := schema.IndexedTorrent{
				Title:         releaseTitle,
				OriginalTitle: title,
				Details:       link,
				Year:          year,
				IMDB:          imdbLink,
				Audio:         magnetAudio,
				MagnetLink:    magnetLink,
				Date:          date,
				InfoHash:      infoHash,
				Trackers:      trackers,
				LeechCount:    peer,
				SeedCount:     seed,
				Size:          mySize,
			}
			chanIndexedTorrent <- ixt
		}(it, magnetLink)
	}

	for i := 0; i < len(magnetLinks); i++ {
		it := <-chanIndexedTorrent
		indexedTorrents = append(indexedTorrents, it)
	}

	return indexedTorrents, nil
}

// starckFilmesDownload is a single download entry found on a starck-filmes detail page.
type starckFilmesDownload struct {
	Magnet string
	Size   string
}

// starckFilmesDetail holds the data extracted from a starck-filmes detail page.
type starckFilmesDetail struct {
	Title     string
	Year      string
	Audio     []schema.Audio
	Sizes     []string
	Downloads []starckFilmesDownload
}

// parseStarckFilmesSearchLinks returns the detail page links listed on a search/listing page.
func parseStarckFilmesSearchLinks(doc *goquery.Document) []string {
	var links []string
	doc.Find(".item div.sub-item > a[href]").Each(func(_ int, s *goquery.Selection) {
		link := strings.TrimSpace(s.AttrOr("href", ""))
		if link != "" {
			links = append(links, link)
		}
	})
	return utils.StableUniq(links)
}

// parseStarckFilmesDetail extracts title, year, audio, sizes and magnet links from a detail page.
func parseStarckFilmesDetail(doc *goquery.Document) starckFilmesDetail {
	var detail starckFilmesDetail

	post := doc.Find(".post").First()
	capa := post.Find(".capa")
	detail.Title = strings.TrimSpace(capa.Find(".post-description > h2").First().Text())

	capa.Find(".post-description p").Each(func(_ int, s *goquery.Selection) {
		// pattern:
		// Nome Original: 28 Weeks Later
		// Lançamento: 2007
		// Duração: 1h 40 min
		// Gênero: Terror, Suspense, Ficção
		// Formato: MKV
		// Tamanho: 2.45 GB
		// Qualidade de Video: 10
		// Qualidade do Audio: 10
		// Idioma: Português | Inglês
		// Legenda: Português, Inglês, Espanhol
		var text strings.Builder
		s.Find("span").Each(func(_ int, span *goquery.Selection) {
			text.WriteString(strings.TrimSpace(span.Text()))
			text.WriteString(" ")
		})
		// "Qualidade do Audio: 10" is a rating, not a language list
		if !strings.HasPrefix(text.String(), "Qualidade") {
			detail.Audio = append(detail.Audio, findAudioFromText(text.String())...)
		}
		if y := findYearFromText(text.String(), detail.Title); y != "" {
			detail.Year = y
		}
		detail.Sizes = append(detail.Sizes, findSizesFromText(text.String())...)
	})
	detail.Sizes = utils.StableUniq(detail.Sizes)

	seen := map[string]bool{}
	addDownload := func(magnetLink string, s *goquery.Selection) {
		if magnetLink == "" || seen[magnetLink] {
			return
		}
		seen[magnetLink] = true
		var size string
		// the current layout shows the size next to each button, e.g. "1080p (2.83 GB)"
		if sizes := findSizesFromText(s.Closest(".btn-down").Text()); len(sizes) > 0 {
			size = sizes[0]
		}
		detail.Downloads = append(detail.Downloads, starckFilmesDownload{Magnet: magnetLink, Size: size})
	}

	buttons := post.Find(".post-buttons")
	buttons.Find("a[href]").Each(func(_ int, s *goquery.Selection) {
		href := strings.TrimSpace(s.AttrOr("href", ""))
		switch {
		case utils.IsMagnetLink(href):
			addDownload(href, s)
		case strings.Contains(href, "xid="):
			// current layout: obfuscated magnet inside a redirector link (https://filmedl.com/?xid=...)
			magnetLink, err := decodeStarckFilmesXid(href)
			if err != nil {
				logging.Warn().Err(err).Str("href", href).Msg("Failed to decode xid link, skipping")
				return
			}
			addDownload(magnetLink, s)
		}
	})
	// legacy layout: obfuscated magnet inside the data-u attribute
	buttons.Find("a[data-u]").Each(func(_ int, s *goquery.Selection) {
		dataU := s.AttrOr("data-u", "")
		magnetLink, err := utils.DecodeStarckDataU(dataU)
		if err != nil {
			logging.Warn().Err(err).Str("data_u", dataU).Msg("Failed to decode data-u attribute, skipping")
			return
		}
		addDownload(magnetLink, s)
	})

	return detail
}

var starckFilmesXidRE = regexp.MustCompile(`[?&]xid=([^&#]+)`)

// decodeStarckFilmesXid decodes the magnet link hidden in the xid query param of the
// download redirector links. The value is a base64 encoded magnet link whose characters
// were shuffled with step 3 (same shuffle used by the old data-u attribute).
func decodeStarckFilmesXid(href string) (string, error) {
	match := starckFilmesXidRE.FindStringSubmatch(href)
	if len(match) < 2 {
		return "", fmt.Errorf("no xid param found")
	}
	// PathUnescape keeps '+' (valid base64 char) untouched
	xid, err := url.PathUnescape(match[1])
	if err != nil {
		xid = match[1]
	}

	unshuffled := starckFilmesUnshuffle(xid, 3)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		decoded, err := enc.DecodeString(unshuffled)
		if err == nil && utils.IsMagnetLink(string(decoded)) {
			return string(decoded), nil
		}
	}
	return "", fmt.Errorf("decoded xid is not a valid magnet link")
}

// starckFilmesUnshuffle reverts the step-based shuffle used by starck-filmes.
func starckFilmesUnshuffle(shuffled string, step int) string {
	runes := []rune(shuffled)
	length := len(runes)
	if length == 0 || step <= 0 {
		return shuffled
	}
	original := make([]rune, length)
	used := make([]bool, length)
	index := 0
	for i := 0; i < length; i++ {
		for used[index] {
			index = (index + 1) % length
		}
		used[index] = true
		original[i] = runes[index]
		index = (index + step) % length
	}
	return string(original)
}

// starck-filmes detail URLs end with the publication date as DD-MM-YYYY, e.g.
// /catalog/a-odisseia-2026-08-11-2026/ -> 08 Nov 2026
var starckFilmesSlugDateRE = regexp.MustCompile(`(\d{2}-\d{2}-\d{4})/?$`)

func getStarckFilmesPublishedDate(link string) time.Time {
	if m := starckFilmesSlugDateRE.FindStringSubmatch(strings.TrimSpace(link)); len(m) > 1 {
		if date, err := time.Parse("02-01-2006", m[1]); err == nil {
			return date.UTC()
		}
	}
	return getPublishedDateFromRawString(link)
}
