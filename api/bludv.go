package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
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

var bludv = IndexerMeta{
	Label:       "bludv",
	URL:         utils.GetIndexerURLFromEnv("INDEXER_BLUDV_URL", "https://bludvfilmes2.xyz/"),
	SearchURL:   "?s=",
	PagePattern: "page/%s",
}

// bludvRESTPath is the WordPress REST API endpoint used to search posts.
// Since Nov/2026 the post HTML pages no longer contain magnet links (they were
// replaced by /gate/go/ redirect pages), but the REST API still returns the
// original post content with the magnet links.
const bludvRESTPath = "wp-json/wp/v2/posts"

// bludvRESTPost is the subset of a WordPress REST API post used by the indexer.
type bludvRESTPost struct {
	ID      int    `json:"id"`
	DateGMT string `json:"date_gmt"`
	Link    string `json:"link"`
	Title   struct {
		Rendered string `json:"rendered"`
	} `json:"title"`
	Content struct {
		Rendered string `json:"rendered"`
	} `json:"content"`
}

// bludvPostData holds the information extracted from a post content.
type bludvPostData struct {
	Title       string
	Link        string
	Date        time.Time
	MagnetLinks []string
	Audio       []schema.Audio
	Year        string
	Sizes       []string
	// LinkSizes maps a magnet link to the size shown right before it (e.g. "WEB-DL 1080p (3.21 GB)")
	LinkSizes map[string]string
	IMDB      string
}

var (
	bludvMagnetHrefRE  = regexp.MustCompile(`href="(magnet:[^"]+)"`)
	bludvSizeInParenRE = regexp.MustCompile(`\((\d+(?:[.,]\d+)?) ?(GB|MB)\)`)
)

// findBluDVLinkSizes maps each magnet link to the last "(N GB|MB)" text found
// between it and the previous magnet link in the given HTML.
func findBluDVLinkSizes(contentHTML string) map[string]string {
	sizes := map[string]string{}
	prev := 0
	for _, loc := range bludvMagnetHrefRE.FindAllStringSubmatchIndex(contentHTML, -1) {
		segment := contentHTML[prev:loc[0]]
		prev = loc[1]
		matches := bludvSizeInParenRE.FindAllStringSubmatch(segment, -1)
		if len(matches) == 0 {
			continue
		}
		last := matches[len(matches)-1]
		link := html.UnescapeString(contentHTML[loc[2]:loc[3]])
		sizes[link] = fmt.Sprintf("%s %s", strings.ReplaceAll(last[1], ",", "."), last[2])
	}
	return sizes
}

func (i *Indexer) HandlerBluDVIndexer(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	metadata := bludv

	defer func() {
		i.metrics.IndexerDuration.WithLabelValues(metadata.Label).Observe(time.Since(start).Seconds())
		i.metrics.IndexerRequests.WithLabelValues(metadata.Label).Inc()
	}()

	ctx := r.Context()
	// supported query params: q, season, episode, page, filter_results
	q := r.URL.Query().Get("q")
	page := r.URL.Query().Get("page")

	indexedTorrents, err := i.searchBluDVREST(ctx, r, metadata, q, page)
	if err != nil {
		// fallback to the legacy HTML scraping (search page + post pages)
		logging.WarnWithRequest(r).Err(err).Str("indexer", metadata.Label).Msg("WP REST API search failed, falling back to HTML scraping")
		indexedTorrents, err = i.searchBluDVHTML(ctx, r, metadata, q, page)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			err = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			if err != nil {
				logging.ErrorWithRequest(r).Err(err).Msg("Failed to encode error response")
			}
			i.metrics.IndexerErrors.WithLabelValues(metadata.Label).Inc()
			return
		}
	}

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

// buildBluDVRESTURL builds the WP REST API URL for searching/listing posts.
func buildBluDVRESTURL(baseURL, q, page string) string {
	params := url.Values{}
	if q != "" {
		params.Set("search", q)
	}
	params.Set("per_page", "20")
	if page != "" {
		params.Set("page", page)
	}
	params.Set("_fields", "id,date_gmt,link,title,content")
	return fmt.Sprintf("%s%s?%s", baseURL, bludvRESTPath, params.Encode())
}

// searchBluDVREST searches posts using the WordPress REST API, which still
// exposes the original magnet links in the post content.
func (i *Indexer) searchBluDVREST(ctx context.Context, r *http.Request, metadata IndexerMeta, q, page string) ([]schema.IndexedTorrent, error) {
	targetURL := buildBluDVRESTURL(metadata.URL, q, page)
	logging.InfoWithRequest(r).Str("target_url", targetURL).Msg("Processing indexer request")

	resp, err := i.requester.GetDocument(ctx, targetURL)
	if err != nil {
		return nil, err
	}
	defer resp.Close()

	body, err := io.ReadAll(resp)
	if err != nil {
		return nil, err
	}

	posts, err := parseBluDVRESTPosts(body)
	if err != nil {
		_ = i.requester.ExpireDocument(ctx, targetURL)
		return nil, err
	}
	if len(posts) == 0 {
		_ = i.requester.ExpireDocument(ctx, targetURL)
	}

	return utils.ParallelFlatMap(posts, func(post bludvRESTPost) ([]schema.IndexedTorrent, error) {
		data, err := parseBluDVRESTPost(post)
		if err != nil {
			return nil, err
		}
		return buildBluDVTorrents(ctx, i, data), nil
	}), nil
}

// parseBluDVRESTPosts decodes a WP REST API posts response. The body can be
// either raw JSON or the JSON wrapped in an HTML <pre> element (as returned by
// FlareSolverr/Chromium).
func parseBluDVRESTPosts(body []byte) ([]bludvRESTPost, error) {
	trimmed := bytes.TrimSpace(body)
	if !bytes.HasPrefix(trimmed, []byte("[")) && !bytes.HasPrefix(trimmed, []byte("{")) {
		doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		pre := doc.Find("pre").First()
		if pre.Length() == 0 {
			return nil, fmt.Errorf("unexpected WP REST API response: no JSON found")
		}
		trimmed = bytes.TrimSpace([]byte(pre.Text()))
	}

	var posts []bludvRESTPost
	if err := json.Unmarshal(trimmed, &posts); err != nil {
		return nil, fmt.Errorf("failed to decode WP REST API response: %w", err)
	}
	return posts, nil
}

// parseBluDVRESTPost extracts the torrent data from a WP REST API post.
func parseBluDVRESTPost(post bludvRESTPost) (bludvPostData, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(
		`<div class="content">` + post.Content.Rendered + `</div>`,
	))
	if err != nil {
		return bludvPostData{}, err
	}

	var date time.Time
	if post.DateGMT != "" {
		date, _ = time.Parse("2006-01-02T15:04:05", post.DateGMT)
	}

	title := cleanBluDVTitle(html.UnescapeString(post.Title.Rendered))
	data := extractBluDVPostData(title, doc.Find("div.content").First())
	data.Link = post.Link
	data.Date = date
	return data, nil
}

func cleanBluDVTitle(title string) string {
	title = strings.TrimSpace(title)
	title = strings.TrimSuffix(title, " - Download")
	title = strings.TrimSuffix(title, " – Download")
	return strings.TrimSpace(title)
}

// searchBluDVHTML is the legacy scraping strategy (search page + post pages).
func (i *Indexer) searchBluDVHTML(ctx context.Context, r *http.Request, metadata IndexerMeta, q, page string) ([]schema.IndexedTorrent, error) {
	q = url.QueryEscape(q)
	targetURL := metadata.URL
	if page != "" {
		targetURL = fmt.Sprintf(fmt.Sprintf("%s%s", targetURL, metadata.PagePattern), page)
	} else {
		targetURL = fmt.Sprintf("%s%s%s", targetURL, metadata.SearchURL, q)
	}

	logging.InfoWithRequest(r).Str("target_url", targetURL).Msg("Processing indexer request")
	resp, err := i.requester.GetDocument(ctx, targetURL)
	if err != nil {
		return nil, err
	}
	defer resp.Close()

	doc, err := goquery.NewDocumentFromReader(resp)
	if err != nil {
		return nil, err
	}

	links := extractBluDVSearchLinks(doc)

	// if no links were indexed, expire the document in cache
	if len(links) == 0 {
		_ = i.requester.ExpireDocument(ctx, targetURL)
	}

	return utils.ParallelFlatMap(links, func(link string) ([]schema.IndexedTorrent, error) {
		return getTorrentsBluDV(ctx, i, link, targetURL)
	}), nil
}

func extractBluDVSearchLinks(doc *goquery.Document) []string {
	var links []string
	doc.Find(".post").Each(func(_ int, s *goquery.Selection) {
		link, ok := s.Find("div.title > a").Attr("href")
		if ok && link != "" {
			links = append(links, link)
		}
	})
	return links
}

func getTorrentsBluDV(ctx context.Context, i *Indexer, link, referer string) ([]schema.IndexedTorrent, error) {
	doc, err := getDocument(ctx, i, link, referer)
	if err != nil {
		return nil, err
	}

	article := doc.Find(".post")
	title := cleanBluDVTitle(article.Find(".title > h1").Text())
	data := extractBluDVPostData(title, article.Find("div.content"))
	data.Link = link
	data.Date = getPublishedDate(doc)

	return buildBluDVTorrents(ctx, i, data), nil
}

// extractBluDVPostData extracts magnets, audio, year, sizes and IMDB link from
// a post content selection (the element wrapping the post body).
func extractBluDVPostData(title string, content *goquery.Selection) bludvPostData {
	data := bludvPostData{Title: title}

	content.Find("a[href^=\"magnet\"]").Each(func(_ int, s *goquery.Selection) {
		magnetLink, _ := s.Attr("href")
		data.MagnetLinks = append(data.MagnetLinks, magnetLink)
	})

	adwareDomains := []string{
		"https://www.seuvideo.xyz",
		"https://www.systemads.org",
		"https://superadsgo.xyz",
	}

	// Process adware links for each domain in the list
	for _, domain := range adwareDomains {
		content.Find(fmt.Sprintf("a[href^=\"%s\"]", domain)).Each(func(_ int, s *goquery.Selection) {
			href, _ := s.Attr("href")
			// extract querysting "id" from url
			parsedUrl, err := url.Parse(href)
			if err != nil {
				logging.Error().Err(err).Str("href", href).Msg("Failed to parse URL")
				return
			}
			magnetLinkDecoded, err := utils.DecodeAdLink(parsedUrl.Query().Get("id"))
			if err != nil {
				logging.Error().Err(err).Str("href", href).Msg("Failed to decode ad link")
				return
			}

			// if decoded magnet link is indeed a magnet link, append it
			if strings.HasPrefix(magnetLinkDecoded, "magnet:") {
				data.MagnetLinks = append(data.MagnetLinks, magnetLinkDecoded)
			} else if !strings.Contains(magnetLinkDecoded, "watch.brplayer") {
				logging.Warn().
					Str("href", href).
					Str("decoded", magnetLinkDecoded).
					Str("indexer", bludv.Label).
					Msg("Link decoding resulted in non-magnet link")
			}
		})
	}
	data.MagnetLinks = utils.StableUniq(data.MagnetLinks)
	if contentHTML, err := content.Html(); err == nil {
		data.LinkSizes = findBluDVLinkSizes(contentHTML)
	}

	content.Find("p").Each(func(_ int, s *goquery.Selection) {
		// pattern:
		// Título Original: Foundation
		// Lançamento: 2023
		// Áudio: Português | Inglês
		// Tamanho: 3.21 GB
		text := s.Text()

		data.Audio = append(data.Audio, findAudioFromText(text)...)
		if y := findYearFromText(text, title); y != "" {
			data.Year = y
		}
		data.Sizes = append(data.Sizes, findSizesFromText(text)...)
	})
	if data.Year == "" {
		data.Year = findYearFromText("", title)
	}
	data.Sizes = utils.StableUniq(data.Sizes)

	// find any link from imdb
	content.Find("a").Each(func(_ int, s *goquery.Selection) {
		link, _ := s.Attr("href")
		if imdbLink, err := getIMDBLink(link); err == nil {
			data.IMDB = imdbLink
		}
	})

	return data
}

// getMagnetExactLength returns the formatted "xl" (exact length) magnet param, if any.
func getMagnetExactLength(magnetLink string) string {
	idx := strings.Index(magnetLink, "?")
	if idx < 0 {
		return ""
	}
	values, err := url.ParseQuery(magnetLink[idx+1:])
	if err != nil {
		return ""
	}
	xl, err := strconv.ParseInt(values.Get("xl"), 10, 64)
	if err != nil || xl <= 0 {
		return ""
	}
	return utils.FormatBytes(xl)
}

// buildBluDVTorrents creates the indexed torrents (one per magnet link),
// fetching seeders/leechers from the trackers.
func buildBluDVTorrents(ctx context.Context, i *Indexer, data bludvPostData) []schema.IndexedTorrent {
	var indexedTorrents []schema.IndexedTorrent
	var chanIndexedTorrent = make(chan schema.IndexedTorrent)

	// for each magnet link, create a new indexed torrent
	for it, magnetLink := range data.MagnetLinks {
		go func(it int, magnetLink string) {
			magnet, err := magnet.ParseMagnetUri(magnetLink)
			if err != nil {
				logging.Error().Err(err).Str("magnet_link", magnetLink).Msg("Failed to parse magnet URI")
			}
			releaseTitle := magnet.DisplayName
			infoHash := magnet.InfoHash.String()
			trackers := magnet.Trackers
			magnetAudio := getAudioFromTitle(releaseTitle, data.Audio)

			peer, seed, err := goscrape.GetLeechsAndSeeds(ctx, i.redis, i.metrics, infoHash, trackers)
			if err != nil {
				logging.Error().Err(err).Str("info_hash", infoHash).Msg("Failed to get leechers and seeders")
			}

			title := processTitle(data.Title, magnetAudio)

			// prefer the exact length from the magnet link, then the size shown next to
			// the link, otherwise if the number of sizes is equal to the number of
			// magnets, assign the size in order
			mySize := getMagnetExactLength(magnetLink)
			if mySize == "" {
				mySize = data.LinkSizes[magnetLink]
			}
			if mySize == "" && len(data.Sizes) == len(data.MagnetLinks) {
				mySize = data.Sizes[it]
			}
			if mySize == "" {
				go func() {
					_, _ = i.magnetMetadataAPI.FetchMetadata(ctx, magnetLink)
				}()
			}

			chanIndexedTorrent <- schema.IndexedTorrent{
				Title:         releaseTitle,
				OriginalTitle: title,
				Details:       data.Link,
				Year:          data.Year,
				IMDB:          data.IMDB,
				Audio:         magnetAudio,
				MagnetLink:    magnetLink,
				Date:          data.Date,
				InfoHash:      infoHash,
				Trackers:      trackers,
				LeechCount:    peer,
				SeedCount:     seed,
				Size:          mySize,
			}
		}(it, magnetLink)
	}

	for range data.MagnetLinks {
		indexedTorrents = append(indexedTorrents, <-chanIndexedTorrent)
	}

	return indexedTorrents
}

func getPublishedDate(document *goquery.Document) time.Time {
	var date time.Time
	//<meta property="article:published_time" content="2019-08-23T13:20:57+00:00">
	datePublished := strings.TrimSpace(document.Find("meta[property=\"article:published_time\"]").AttrOr("content", ""))

	if datePublished != "" {
		date, _ = time.Parse(time.RFC3339, datePublished)
	}

	return date
}
