package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"github.com/felipemarinho97/torrent-indexer/logging"
	"github.com/felipemarinho97/torrent-indexer/magnet"
	"github.com/felipemarinho97/torrent-indexer/schema"
	goscrape "github.com/felipemarinho97/torrent-indexer/scrape"
	"github.com/felipemarinho97/torrent-indexer/utils"
)

var rede_torrent = IndexerMeta{
	Label: "rede_torrent",
	URL:   utils.GetIndexerURLFromEnv("INDEXER_REDE_TORRENT_URL", "https://redestorrents.com/"),
	// the redesigned search form submits "busca"; the legacy "?s=" parameter is
	// ignored and answers every query with the homepage listing. As of 2026-11
	// the form no longer carries the anti-bot "token" field, so a plain GET works.
	SearchURL:   "index.php?busca=",
	PagePattern: "pagina/%s/",
}

func (i *Indexer) HandlerRedeTorrentIndexer(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	metadata := rede_torrent

	defer func() {
		i.metrics.IndexerDuration.WithLabelValues(metadata.Label).Observe(time.Since(start).Seconds())
		i.metrics.IndexerRequests.WithLabelValues(metadata.Label).Inc()
	}()

	ctx := r.Context()
	// supported query params: q, season, episode, page, filter_results
	q := r.URL.Query().Get("q")
	page := r.URL.Query().Get("page")

	// search results are not paginated (the site returns a single page), so
	// "page" only applies to the listing
	targetURL := metadata.URL
	if q != "" {
		targetURL = fmt.Sprintf("%s%s%s", targetURL, metadata.SearchURL, url.QueryEscape(q))
	} else if page != "" {
		targetURL = fmt.Sprintf(fmt.Sprintf("%s%s", targetURL, metadata.PagePattern), page)
	}

	logging.InfoWithRequest(r).Str("target_url", targetURL).Msg("Processing indexer request")
	resp, err := i.requester.GetDocument(ctx, targetURL)
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

	links := extractLinksRedeTorrent(doc)

	// if no links were indexed, expire the document in cache
	if len(links) == 0 {
		_ = i.requester.ExpireDocument(ctx, targetURL)
	}

	// extract each torrent link
	indexedTorrents := utils.ParallelFlatMap(links, func(link string) ([]schema.IndexedTorrent, error) {
		return getTorrentsRedeTorrent(ctx, i, link, targetURL)
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

func getTorrentsRedeTorrent(ctx context.Context, i *Indexer, link, referer string) ([]schema.IndexedTorrent, error) {
	var indexedTorrents []schema.IndexedTorrent
	doc, err := getDocument(ctx, i, link, referer)
	if err != nil {
		return nil, err
	}

	// the details page no longer has a parseable <h1> or #informacoes block, so
	// title, year and imdb come from the schema.org Movie block the new theme
	// emits for SEO, with the breadcrumb and <h1> as fallbacks.
	title, year, imdbLink := extractMovieJSONLDRedeTorrent(doc)
	if title == "" {
		title = strings.TrimSpace(doc.Find(".breadcrumb-item.active [itemprop=\"name\"]").First().Text())
	}
	if title == "" {
		title = strings.TrimSpace(doc.Find("h1").First().Text())
	}
	if title == "" {
		return nil, fmt.Errorf("could not extract title from %s", link)
	}

	textContent := doc.Text()
	date := getPublishedDateFromMeta(doc)

	magnetLinks, sizeByMagnet := extractMagnetsRedeTorrent(doc)

	audio := extractAudioRedeTorrent(doc)
	if len(audio) == 0 {
		audio = findAudioFromText(textContent)
	}
	if year == "" {
		year = extractYearRedeTorrent(doc)
	}
	if year == "" {
		year = findYearFromText(textContent, title)
	}

	// find any link from imdb, fallback if JSON-LD didn't have "sameAs"
	if imdbLink == "" {
		doc.Find("a").EachWithBreak(func(i int, s *goquery.Selection) bool {
			href, _ := s.Attr("href")
			_imdbLink, err := getIMDBLink(href)
			if err == nil {
				imdbLink = _imdbLink
				return false
			}
			return true
		})
	}

	var chanIndexedTorrent = make(chan schema.IndexedTorrent)

	// for each magnet link, create a new indexed torrent
	for _, magnetLink := range magnetLinks {
		go func(magnetLink string) {
			magnet, err := magnet.ParseMagnetUri(magnetLink)
			if err != nil {
				logging.Error().Err(err).Str("magnet_link", magnetLink).Msg("Failed to parse magnet URI")
			}
			releaseTitle := magnet.DisplayName
			infoHash := magnet.InfoHash.String()
			trackers := magnet.Trackers
			magnetAudio := getAudioFromTitle(releaseTitle, audio)

			peer, seed, err := goscrape.GetLeechsAndSeeds(ctx, i.redis, i.metrics, infoHash, trackers)
			if err != nil {
				logging.Error().Err(err).Str("info_hash", infoHash).Msg("Failed to get leechers and seeders")
			}

			title := processTitle(title, magnetAudio)

			mySize := sizeByMagnet[magnetLink]
			// if the row didn't print a size, the magnet's "xl" param (exact
			// byte length) is the next best source.
			if mySize == "" && magnet.Params != nil {
				if xl := magnet.Params.Get("xl"); xl != "" {
					mySize = formatBytesToSizeRedeTorrent(xl)
				}
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
		}(magnetLink)
	}

	for i := 0; i < len(magnetLinks); i++ {
		it := <-chanIndexedTorrent
		indexedTorrents = append(indexedTorrents, it)
	}

	return indexedTorrents, nil
}

// extractAudioRedeTorrent reads the languages off the "Idioma / Áudio" spec
// card the redesigned details page prints. The card holds the value without the
// "Idioma:" label findAudioFromText keys on, so the label is put back before
// parsing — that keeps the language table and the separator handling in the one
// place that already owns them.
func extractAudioRedeTorrent(doc *goquery.Document) []schema.Audio {
	var audio []schema.Audio
	doc.Find(".spec-text").EachWithBreak(func(i int, s *goquery.Selection) bool {
		// "Qualidade de Áudio" is a neighbouring card holding a score, so the
		// label has to match on "Idioma" rather than on "Áudio"
		if !strings.Contains(s.Find("small").Text(), "Idioma") {
			return true
		}
		audio = findAudioFromText("Idioma: " + strings.TrimSpace(s.Find("strong").Text()))
		return false
	})
	return audio
}

// extractMagnetsRedeTorrent collects the magnet links of a details page in
// document order, together with the size the page prints for each one.
//
// Sizes have to be read per magnet. Every download option sits in its own
// ".download-row", whose ".download-name" ends with that release's size
// ("1ª OPÇÃO DOWNLOAD TORRENT DUBLADO 720P / 796 MB"), while the
// page repeats one of those sizes further down in a "Tamanho do Arquivo" spec
// card. Collecting sizes from the whole document and pairing them with the
// magnets by index therefore lines the wrong size up against the wrong release.
func extractMagnetsRedeTorrent(doc *goquery.Document) ([]string, map[string]string) {
	var magnetLinks []string
	sizeByMagnet := make(map[string]string)

	doc.Find(".download-row").Each(func(i int, s *goquery.Selection) {
		magnetLink, exists := s.Find("a[href^=\"magnet\"]").First().Attr("href")
		if !exists {
			return
		}
		magnetLinks = append(magnetLinks, magnetLink)
		if sizes := findSizesFromText(s.Find(".download-name").Text()); len(sizes) > 0 {
			sizeByMagnet[magnetLink] = sizes[len(sizes)-1]
		}
	})

	// pick up any magnet that isn't inside a ".download-row"
	doc.Find("a[href^=\"magnet\"]").Each(func(i int, s *goquery.Selection) {
		if magnetLink, exists := s.Attr("href"); exists {
			magnetLinks = append(magnetLinks, magnetLink)
		}
	})
	magnetLinks = utils.StableUniq(magnetLinks)

	// some pages print no size on the download row, only the page-wide spec
	// card. That value can be attributed to a release only when the page offers
	// a single one.
	if len(magnetLinks) == 1 && sizeByMagnet[magnetLinks[0]] == "" {
		doc.Find(".spec-text").EachWithBreak(func(i int, s *goquery.Selection) bool {
			if !strings.Contains(s.Find("small").Text(), "Tamanho") {
				return true
			}
			if sizes := findSizesFromText(s.Find("strong").Text()); len(sizes) > 0 {
				sizeByMagnet[magnetLinks[0]] = sizes[0]
			}
			return false
		})
	}

	return magnetLinks, sizeByMagnet
}

// extractMovieJSONLDRedeTorrent reads the schema.org "Movie"/"TVSeries" JSON-LD block that the
// redestorrents.com theme emits for SEO and returns title, year and IMDB link.
// Returns empty strings for any field it can't find.
func extractMovieJSONLDRedeTorrent(doc *goquery.Document) (title, year, imdbLink string) {
	doc.Find("script[type=\"application/ld+json\"]").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		var data struct {
			Type          string `json:"@type"`
			Name          string `json:"name"`
			SameAs        string `json:"sameAs"`
			CopyrightYear any    `json:"copyrightYear"` // string or number
		}
		if err := json.Unmarshal([]byte(s.Text()), &data); err != nil {
			return true // continue to next script tag
		}
		// films are "Movie", series pages (e.g. Animatrix) are "TVSeries"
		switch data.Type {
		case "Movie", "TVSeries", "TVSeason", "TVEpisode":
		default:
			return true
		}
		title = strings.TrimSpace(data.Name)
		if data.CopyrightYear != nil {
			year = strings.TrimSpace(fmt.Sprint(data.CopyrightYear))
		}
		if l, err := getIMDBLink(data.SameAs); err == nil {
			imdbLink = l
		}
		return false // found it, stop
	})
	return
}

// formatBytesToSizeRedeTorrent converts a byte-length string (as found in a magnet's
// "xl" param) into a human readable "X.XX GB"/"X.XX MB" string.
func formatBytesToSizeRedeTorrent(byteStr string) string {
	bytes, err := strconv.ParseFloat(byteStr, 64)
	if err != nil || bytes <= 0 {
		return ""
	}
	const unit = 1024.0
	switch {
	case bytes >= unit*unit*unit:
		return fmt.Sprintf("%.2f GB", bytes/(unit*unit*unit))
	case bytes >= unit*unit:
		return fmt.Sprintf("%.2f MB", bytes/(unit*unit))
	default:
		return fmt.Sprintf("%.2f KB", bytes/unit)
	}
}

var redeTorrentYearRe = regexp.MustCompile(`\((\d{4})\)`)

// extractYearRedeTorrent falls back to the release year the page prints in its
// heading ("Animatrix - A História Antes de Matrix Torrent (2003) Dual Áudio")
// or in the <time itemprop="datePublished" datetime="2003-01-01"> info pill.
func extractYearRedeTorrent(doc *goquery.Document) string {
	for _, text := range []string{
		doc.Find("h1.movie-title").First().Text(),
		doc.Find("meta[property=\"og:title\"]").AttrOr("content", ""),
	} {
		if m := redeTorrentYearRe.FindStringSubmatch(text); len(m) == 2 {
			return m[1]
		}
	}
	if dt := doc.Find("time[itemprop=\"datePublished\"]").First().AttrOr("datetime", ""); len(dt) >= 4 {
		if _, err := strconv.Atoi(dt[:4]); err == nil {
			return dt[:4]
		}
	}
	return ""
}

// extractLinksRedeTorrent returns the details-page links of a listing or search
// results page: both are a grid of <a class="cover-link"><article class="custom-card">.
func extractLinksRedeTorrent(doc *goquery.Document) []string {
	var links []string
	doc.Find("a.cover-link").Each(func(_ int, s *goquery.Selection) {
		if link, exists := s.Attr("href"); exists && link != "" {
			links = append(links, link)
		}
	})
	return utils.StableUniq(links)
}
