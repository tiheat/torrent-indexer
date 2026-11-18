package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/felipemarinho97/torrent-indexer/logging"
	"github.com/felipemarinho97/torrent-indexer/magnet"
	"github.com/felipemarinho97/torrent-indexer/schema"
	goscrape "github.com/felipemarinho97/torrent-indexer/scrape"
	"github.com/felipemarinho97/torrent-indexer/utils"
)

// vacaTorrent scrapes Vaqueiro Filmes (formerly VacaTorrent / vacatorrentmov.com).
// The site still runs the same "VacaTorrent" WordPress theme, so the AJAX search
// endpoint is unchanged. Download links, however, are now hidden behind an
// ad-safelink ("/enc/go.php?id=..."), which is resolved by
// resolveVacaTorrentLink.
var vacaTorrent = IndexerMeta{
	Label:       "vaca_torrent",
	URL:         utils.GetIndexerURLFromEnv("INDEXER_VACA_TORRENT_URL", "https://vaqueirofilmes.com/"),
	SearchURL:   "wp-admin/admin-ajax.php",
	PagePattern: "page/%s",
}

func (i *Indexer) HandlerVacaTorrentIndexer(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	metadata := vacaTorrent

	defer func() {
		i.metrics.IndexerDuration.WithLabelValues(metadata.Label).Observe(time.Since(start).Seconds())
		i.metrics.IndexerRequests.WithLabelValues(metadata.Label).Inc()
	}()

	ctx := r.Context()
	// supported query params: q, season, episode, page, filter_results
	q := r.URL.Query().Get("q")
	page := r.URL.Query().Get("page")

	if page == "" {
		page = "1"
	}

	writeError := func(err error) {
		w.WriteHeader(http.StatusInternalServerError)
		if encErr := json.NewEncoder(w).Encode(map[string]string{"error": err.Error()}); encErr != nil {
			logging.ErrorWithRequest(r).Err(encErr).Msg("Failed to encode error response")
		}
		i.metrics.IndexerErrors.WithLabelValues(metadata.Label).Inc()
	}

	var doc *goquery.Document
	var err error
	var targetURL string

	if q != "" {
		// the search page only renders results through this AJAX endpoint
		targetURL = fmt.Sprintf("%s%s", metadata.URL, metadata.SearchURL)
		logging.InfoWithRequest(r).Str("target_url", targetURL).Str("query", q).Msg("Processing indexer request")
		doc, err = postSearchVacaTorrent(ctx, metadata.URL, targetURL, q, page)
		if err != nil {
			writeError(err)
			return
		}
	} else {
		// For home page or pagination
		targetURL = metadata.URL
		if page != "1" {
			targetURL = fmt.Sprintf(fmt.Sprintf("%s%s", targetURL, metadata.PagePattern), page)
		}

		logging.InfoWithRequest(r).Str("target_url", targetURL).Msg("Processing indexer request")
		resp, err := i.requester.GetDocument(ctx, targetURL)
		if err != nil {
			writeError(err)
			return
		}
		defer resp.Close()

		doc, err = goquery.NewDocumentFromReader(resp)
		if err != nil {
			writeError(err)
			return
		}
	}

	links := extractLinksVacaTorrent(doc)

	// if no links were indexed, expire the document in cache
	if len(links) == 0 {
		_ = i.requester.ExpireDocument(ctx, targetURL)
	}

	// extract each torrent link
	indexedTorrents := utils.ParallelFlatMap(links, func(link string) ([]schema.IndexedTorrent, error) {
		return getTorrentsVacaTorrent(ctx, i, link, targetURL)
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

// extractLinksVacaTorrent returns the detail page links of a listing (home page
// or AJAX search results), in order and without duplicates.
func extractLinksVacaTorrent(doc *goquery.Document) []string {
	var links []string
	seen := map[string]bool{}
	doc.Find(".i-tem_ht").Each(func(_ int, s *goquery.Selection) {
		link, exists := s.Find("a").First().Attr("href")
		link = strings.TrimSpace(link)
		if exists && link != "" && !seen[link] {
			seen[link] = true
			links = append(links, link)
		}
	})
	return links
}

// VacaTorrentAjaxResponse represents the JSON response from the WordPress AJAX endpoint
type VacaTorrentAjaxResponse struct {
	HTML        string `json:"html"`
	Total       int    `json:"total"`
	Pages       int    `json:"pages"`
	CountTodos  int    `json:"count_todos"`
	CountFilme  int    `json:"count_filme"`
	CountSerie  int    `json:"count_serie"`
	CountHQs    int    `json:"count_hqs"`
	CountMangas int    `json:"count_mangas"`
}

func postSearchVacaTorrent(ctx context.Context, baseURL, targetURL, query, page string) (*goquery.Document, error) {
	// Create multipart form data (same fields the site's search page sends)
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	_ = writer.WriteField("action", "filtrar_busca")
	_ = writer.WriteField("s", query)
	_ = writer.WriteField("tipo", "todos")
	_ = writer.WriteField("paged", page)
	_ = writer.WriteField("lang", "pt-BR")

	err := writer.Close()
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", targetURL, body)
	if err != nil {
		return nil, err
	}

	origin := strings.TrimSuffix(baseURL, "/")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("User-Agent", utils.SpoofedUserAgent)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "pt-BR,pt;q=0.9,en-US;q=0.8,en;q=0.7")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", fmt.Sprintf("%s/?s=%s", origin, url.QueryEscape(query)))
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	client := &http.Client{
		Timeout: 30 * time.Second,
		// a redirect would turn the POST into a GET of some HTML page
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return parseVacaTorrentAjaxResponse(resp.StatusCode, resp.Header.Get("Location"), bodyBytes)
}

// parseVacaTorrentAjaxResponse parses the AJAX search response into a document.
func parseVacaTorrentAjaxResponse(status int, location string, bodyBytes []byte) (*goquery.Document, error) {
	if status >= 300 && status < 400 {
		return nil, fmt.Errorf("search endpoint redirected (status %d) to %q, the site domain probably changed: set INDEXER_VACA_TORRENT_URL", status, location)
	}

	var ajaxResp VacaTorrentAjaxResponse
	if err := json.Unmarshal(bodyBytes, &ajaxResp); err != nil {
		snippet := strings.TrimSpace(string(bodyBytes))
		if len(snippet) > 120 {
			snippet = snippet[:120]
		}
		return nil, fmt.Errorf("failed to parse JSON response (status %d): %w; body starts with %q", status, err, snippet)
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(ajaxResp.HTML))
	if err != nil {
		return nil, fmt.Errorf("failed to parse HTML: %w", err)
	}
	return doc, nil
}

// vacaTorrentLink is a download button found in a page.
type vacaTorrentLink struct {
	// URL is either a magnet link or a safelink ("/enc/go.php?id=...")
	URL string
	// Label is the button / paragraph text, e.g. "Português | Inglês • 1080p • 2.63 GB"
	Label string
	// Episode is the episode number when the link comes from a season page
	Episode string
}

// vacaTorrentPage contains the data extracted from a detail page.
type vacaTorrentPage struct {
	Title  string
	Year   string
	IMDB   string
	Audio  []schema.Audio
	Sizes  []string
	Season string
	Date   time.Time
	Links  []vacaTorrentLink
	// LinksPage is the "/movie-links/<id>/" page of new-style movie pages
	LinksPage string
	// SeasonPage is the latest season page of new-style tv-show pages
	SeasonPage string
}

var (
	vacaYearInParensRE  = regexp.MustCompile(`\((\d{4})\)`)
	vacaLancamentoRE    = regexp.MustCompile(`Lançamento[^:\n]*:\s*(\d{4})`)
	vacaSeasonTextRE    = regexp.MustCompile(`(?i)(?:temporada|season)[\s-]*(\d+)`)
	vacaSeasonURLRE     = regexp.MustCompile(`/season/[^/]*?(\d+)/?$`)
	vacaLabelSizeRE     = regexp.MustCompile(`(\d+(?:[.,]\d+)?)\s?(GB|MB)`)
	vacaLabelQualityRE  = regexp.MustCompile(`(?i)\b(480p|720p|1080p|2160p|4k)\b.*`)
	vacaTitleSuffixesRE = regexp.MustCompile(`(?i)\s+(torrent|download)\s*$`)
)

// parseVacaTorrentPage extracts metadata and download links from a detail page.
// It handles the three page kinds of the site: new movie pages (links live in a
// separate "/movie-links/" page), tv-show pages (links live in season pages) and
// legacy posts (links in the post content).
func parseVacaTorrentPage(doc *goquery.Document, pageURL string) vacaTorrentPage {
	var p vacaTorrentPage

	titleSel := doc.Find(".custom-main-title").First()
	p.Year = extractYearVacaTorrent(titleSel.Find(".release_date").Text())
	titleSel.Find(".release_date").Remove()
	p.Title = cleanTitleVacaTorrent(titleSel.Text())
	if p.Title == "" {
		p.Title = cleanTitleVacaTorrent(doc.Find("h1").First().Text())
	}

	doc.Find(".col-left ul li, .content p").Each(func(_ int, s *goquery.Selection) {
		text := s.Text()
		if p.Year == "" {
			if m := vacaLancamentoRE.FindStringSubmatch(text); len(m) > 1 {
				p.Year = m[1]
			}
		}
		if len(p.Audio) == 0 {
			// line by line, so "Qualidade do Audio: 10" does not shadow "Idioma: ..."
			for _, line := range strings.Split(text, "\n") {
				if strings.Contains(line, "Qualidade") {
					continue
				}
				if a := findAudioFromText(line); len(a) > 0 {
					p.Audio = a
					break
				}
			}
		}
		if strings.Contains(text, "Tamanho") {
			p.Sizes = append(p.Sizes, findSizesFromText(text)...)
		}
	})
	p.Sizes = utils.StableUniq(p.Sizes)

	doc.Find(`a[href*="imdb.com"]`).EachWithBreak(func(_ int, s *goquery.Selection) bool {
		if l, err := getIMDBLink(s.AttrOr("href", "")); err == nil {
			p.IMDB = l
			return false
		}
		return true
	})

	p.Date = getPublishedDateFromMeta(doc)

	// season pages: "/tv/<show>/season/temporada-1/" with h1 "Temporada 1"
	if strings.Contains(pageURL, "/season/") {
		if m := vacaSeasonTextRE.FindStringSubmatch(doc.Find(".ss-hero-title").First().Text()); len(m) > 1 {
			p.Season = m[1]
		} else if m := vacaSeasonURLRE.FindStringSubmatch(pageURL); len(m) > 1 {
			p.Season = m[1]
		}
	}

	p.Links = extractDownloadLinksVacaTorrent(doc)

	doc.Find(`a[href*="/movie-links/"]`).EachWithBreak(func(_ int, s *goquery.Selection) bool {
		p.LinksPage = s.AttrOr("href", "")
		return false
	})
	doc.Find(`a.ls-card[href*="/season/"], a[href*="/tv/"][href*="/season/"]`).EachWithBreak(func(_ int, s *goquery.Selection) bool {
		p.SeasonPage = s.AttrOr("href", "")
		return false
	})

	return p
}

func extractYearVacaTorrent(text string) string {
	if m := vacaYearInParensRE.FindStringSubmatch(text); len(m) > 1 {
		return m[1]
	}
	return ""
}

func cleanTitleVacaTorrent(title string) string {
	title = strings.Join(strings.Fields(title), " ")
	title = strings.TrimSpace(strings.Split(title, " (")[0])
	return strings.TrimSpace(vacaTitleSuffixesRE.ReplaceAllString(title, ""))
}

// extractDownloadLinksVacaTorrent finds magnet and safelink download buttons.
func extractDownloadLinksVacaTorrent(doc *goquery.Document) []vacaTorrentLink {
	var links []vacaTorrentLink
	seen := map[string]bool{}
	doc.Find(`a[href^="magnet:"], a[href*="/enc/go.php"]`).Each(func(_ int, s *goquery.Selection) {
		href := strings.TrimSpace(s.AttrOr("href", ""))
		if href == "" || seen[href] {
			return
		}
		seen[href] = true

		label := strings.Join(strings.Fields(s.Text()), " ")
		if label == "" {
			// legacy posts: the paragraph holds e.g. "SERVIDOR PARA DOWNLOAD WEB-DL 1080p (2.17 GB)"
			label = strings.Join(strings.Fields(s.Closest("p").Text()), " ")
		}
		if label == "" {
			label = strings.TrimSpace(s.Find("img").AttrOr("alt", ""))
		}

		episode := ""
		if ep := s.Closest(".ss-ep"); ep.Length() > 0 {
			episode = strings.TrimSpace(ep.Find(".ss-ep-num").First().Text())
		}

		links = append(links, vacaTorrentLink{URL: href, Label: label, Episode: episode})
	})
	return links
}

// parseVacaTorrentLinkLabel parses a label like "Português | Inglês • 1080p • 2.63 GB".
func parseVacaTorrentLinkLabel(label string) (audio []schema.Audio, quality, size string) {
	label = html.UnescapeString(label)
	parts := strings.Split(label, "•")
	if len(parts) > 1 {
		for _, lang := range strings.Split(parts[0], "|") {
			if a := schema.GetAudioFromString(strings.TrimSpace(lang)); a != nil {
				audio = append(audio, *a)
			}
		}
	}
	if m := vacaLabelSizeRE.FindString(label); m != "" {
		size = m
	}
	for _, part := range parts {
		if m := vacaLabelQualityRE.FindString(part); m != "" {
			quality = strings.TrimSpace(strings.TrimRight(vacaLabelSizeRE.ReplaceAllString(m, ""), " ()"))
			break
		}
	}
	return audio, quality, size
}

// extractMagnetFromVacaTorrentReceber extracts the destination link from the
// last safelink step page: <body data-link="base64(link)">.
func extractMagnetFromVacaTorrentReceber(doc *goquery.Document) (string, error) {
	encoded := strings.TrimSpace(doc.Find("body").AttrOr("data-link", ""))
	if encoded == "" {
		return "", fmt.Errorf("data-link not found in safelink page")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("failed to decode data-link: %w", err)
	}
	return html.UnescapeString(strings.TrimSpace(string(decoded))), nil
}

// vacaTorrentResolveSem limits the number of concurrent safelink resolutions.
var vacaTorrentResolveSem = make(chan struct{}, 8)

// vacaTorrentMaxLinksPerPage limits how many download links of a single page
// are resolved (enough for a full season page).
const vacaTorrentMaxLinksPerPage = 60

// resolveVacaTorrentLink resolves a "/enc/go.php?id=<id>" safelink.
//
// go.php is protected by a Cloudflare challenge, but it only stores <id> in a
// cookie and redirects to an ad page that links to "/enc2/receber.php?enc=<id>".
// receber.php accepts the same <id>, sets a cookie and redirects to another ad
// page that carries the destination link base64-encoded in <body data-link>.
// Only magnet destinations are returned (MEGA/MediaFire/etc. are skipped).
func resolveVacaTorrentLink(ctx context.Context, i *Indexer, goLink string) (string, error) {
	u, err := url.Parse(html.UnescapeString(goLink))
	if err != nil {
		return "", err
	}
	id := u.Query().Get("id")
	if id == "" {
		return "", fmt.Errorf("safelink without id: %s", goLink)
	}

	cacheKey := "vaca_torrent:link:" + id
	if cached, err := i.redis.Get(ctx, cacheKey); err == nil {
		return string(cached), nil
	}

	select {
	case vacaTorrentResolveSem <- struct{}{}:
		defer func() { <-vacaTorrentResolveSem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}

	receberURL := fmt.Sprintf("%s://%s/enc2/receber.php?enc=%s", u.Scheme, u.Host, url.QueryEscape(id))
	jar, err := cookiejar.New(nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}

	req, err := http.NewRequestWithContext(ctx, "GET", receberURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", utils.SpoofedUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Referer", fmt.Sprintf("%s://%s/", u.Scheme, u.Host))

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code %d resolving safelink", resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return "", err
	}
	link, err := extractMagnetFromVacaTorrentReceber(doc)
	if err != nil {
		return "", err
	}

	// cache non-magnet destinations too, so they are not resolved again
	_ = i.redis.Set(ctx, cacheKey, []byte(link))
	return link, nil
}

func getTorrentsVacaTorrent(ctx context.Context, i *Indexer, link, referer string) ([]schema.IndexedTorrent, error) {
	doc, err := getDocument(ctx, i, link, referer)
	if err != nil {
		return nil, err
	}
	page := parseVacaTorrentPage(doc, link)

	// new-style movie pages: the download buttons are in "/movie-links/<id>/"
	if len(page.Links) == 0 && page.LinksPage != "" {
		linksDoc, err := getDocument(ctx, i, page.LinksPage, link)
		if err != nil {
			return nil, err
		}
		page.Links = extractDownloadLinksVacaTorrent(linksDoc)
	}

	// tv-show pages: the download buttons are in the (latest) season page
	if len(page.Links) == 0 && page.SeasonPage != "" {
		seasonDoc, err := getDocument(ctx, i, page.SeasonPage, link)
		if err != nil {
			return nil, err
		}
		seasonPage := parseVacaTorrentPage(seasonDoc, page.SeasonPage)
		page.Links = seasonPage.Links
		page.Season = seasonPage.Season
	}

	if len(page.Links) == 0 {
		return nil, nil
	}

	// some legacy "listão" / collection posts have hundreds of links to old,
	// mostly unrelated releases; resolving all of them would take minutes and
	// hammer the site, and a truncated subset would be arbitrary, so skip them
	if len(page.Links) > vacaTorrentMaxLinksPerPage {
		logging.Warn().Str("link", link).Int("links", len(page.Links)).Int("max", vacaTorrentMaxLinksPerPage).Msg("Too many download links in vaca_torrent page, skipping it")
		return nil, nil
	}

	if page.Date.IsZero() {
		yearInt, _ := strconv.Atoi(page.Year)
		page.Date = time.Date(yearInt, time.January, 1, 0, 0, 0, 0, time.UTC)
	}

	// resolve the safelinks to magnet links
	type resolved struct {
		link   vacaTorrentLink
		magnet string
	}
	results := make([]resolved, len(page.Links))
	var wg sync.WaitGroup
	for idx, l := range page.Links {
		wg.Add(1)
		go func(idx int, l vacaTorrentLink) {
			defer wg.Done()
			target := l.URL
			if !strings.HasPrefix(target, "magnet:") {
				var err error
				target, err = resolveVacaTorrentLink(ctx, i, l.URL)
				if err != nil {
					logging.Error().Err(err).Str("link", link).Msg("Failed to resolve vaca_torrent safelink")
					return
				}
			}
			if strings.HasPrefix(target, "magnet:") {
				results[idx] = resolved{link: l, magnet: target}
			}
		}(idx, l)
	}
	wg.Wait()

	var magnets []resolved
	seen := map[string]bool{}
	for _, res := range results {
		if res.magnet != "" && !seen[res.magnet] {
			seen[res.magnet] = true
			magnets = append(magnets, res)
		}
	}

	indexed := make([]schema.IndexedTorrent, len(magnets))
	wg = sync.WaitGroup{}
	for idx, res := range magnets {
		wg.Add(1)
		go func(idx int, res resolved) {
			defer wg.Done()
			res.magnet = fixVacaTorrentMagnetTrackers(res.magnet)
			m, err := magnet.ParseMagnetUri(res.magnet)
			if err != nil {
				logging.Error().Err(err).Str("magnet_link", res.magnet).Msg("Failed to parse magnet URI")
				return
			}
			infoHash := m.InfoHash.String()
			trackers := m.Trackers

			labelAudio, quality, labelSize := parseVacaTorrentLinkLabel(res.link.Label)
			contentAudio := page.Audio
			if len(labelAudio) > 0 {
				contentAudio = labelAudio
			}

			releaseTitle := strings.TrimSpace(html.UnescapeString(m.DisplayName))
			var magnetAudio []schema.Audio
			if releaseTitle == "" {
				// no display name: trust the languages shown next to the link
				magnetAudio = contentAudio
				releaseTitle = buildVacaTorrentReleaseTitle(page, res.link, quality, magnetAudio)
			} else {
				magnetAudio = getAudioFromTitle(releaseTitle, contentAudio)
			}

			peer, seed, err := goscrape.GetLeechsAndSeeds(ctx, i.redis, i.metrics, infoHash, trackers)
			if err != nil {
				logging.Error().Err(err).Str("info_hash", infoHash).Msg("Failed to get leechers and seeders")
			}

			// prefer the exact length from the magnet, then the size shown in the
			// button, then the sizes listed in the post if they match the magnets
			mySize := getVacaTorrentMagnetLength(res.magnet)
			if mySize == "" {
				mySize = labelSize
			}
			if mySize == "" && len(page.Sizes) == len(magnets) {
				mySize = page.Sizes[idx]
			}
			if mySize == "" {
				go func() {
					_, _ = i.magnetMetadataAPI.FetchMetadata(ctx, res.magnet)
				}()
			}

			indexed[idx] = schema.IndexedTorrent{
				Title:         releaseTitle,
				OriginalTitle: processVacaTorrentTitle(page.Title, magnetAudio, page.Season),
				Details:       link,
				Year:          page.Year,
				IMDB:          page.IMDB,
				Audio:         magnetAudio,
				MagnetLink:    res.magnet,
				Date:          page.Date,
				InfoHash:      infoHash,
				Trackers:      trackers,
				LeechCount:    peer,
				SeedCount:     seed,
				Size:          mySize,
			}
		}(idx, res)
	}
	wg.Wait()

	var indexedTorrents []schema.IndexedTorrent
	for _, it := range indexed {
		// Skip empty torrents (failed to parse)
		if it.InfoHash != "" {
			indexedTorrents = append(indexedTorrents, it)
		}
	}
	return indexedTorrents, nil
}

// buildVacaTorrentReleaseTitle builds a release title for magnets without a
// display name, e.g. "Bom Garoto 2025 1080p DUAL" or "A Ilha do Tesouro S01E03 1080p".
// The DUAL/DUBLADO marker keeps the audio post-processor from dropping Portuguese.
func buildVacaTorrentReleaseTitle(page vacaTorrentPage, l vacaTorrentLink, quality string, audio []schema.Audio) string {
	parts := []string{page.Title}
	if page.Season != "" {
		se := fmt.Sprintf("S%02s", page.Season)
		if l.Episode != "" {
			se += fmt.Sprintf("E%02s", l.Episode)
		}
		parts = append(parts, se)
	} else if page.Year != "" {
		parts = append(parts, page.Year)
	}
	if quality != "" {
		parts = append(parts, quality)
	}
	if slices.Contains(audio, schema.AudioPortuguese) {
		if len(audio) > 1 {
			parts = append(parts, "DUAL")
		} else {
			parts = append(parts, "DUBLADO")
		}
	}
	return strings.Join(parts, " ")
}

// vacaBrokenTrackerRE matches trackers mangled by the site, which strips ":" and
// "/" from them, e.g. "udptracker.opentrackr.org1337announce".
var vacaBrokenTrackerRE = regexp.MustCompile(`^(udp|https?)([a-z0-9.-]+?\.[a-z]{2,})(\d{2,5})(announce|anunciar)$`)

// fixVacaTorrentMagnetTrackers repairs mangled tracker URLs of a magnet link.
func fixVacaTorrentMagnetTrackers(magnetLink string) string {
	idx := strings.Index(magnetLink, "?")
	if idx < 0 || !strings.Contains(magnetLink, "tr=") {
		return magnetLink
	}
	params := strings.Split(magnetLink[idx+1:], "&")
	changed := false
	for k, param := range params {
		value, ok := strings.CutPrefix(param, "tr=")
		if !ok || strings.Contains(value, "%3A") || strings.Contains(value, "://") {
			continue
		}
		m := vacaBrokenTrackerRE.FindStringSubmatch(value)
		if m == nil {
			continue
		}
		fixed := fmt.Sprintf("%s://%s:%s/announce", m[1], m[2], m[3])
		params[k] = "tr=" + url.QueryEscape(fixed)
		changed = true
	}
	if !changed {
		return magnetLink
	}
	return magnetLink[:idx+1] + strings.Join(params, "&")
}

// getVacaTorrentMagnetLength returns the formatted "xl" (exact length) of a magnet.
func getVacaTorrentMagnetLength(magnetLink string) string {
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

func processVacaTorrentTitle(title string, audio []schema.Audio, season string) string {
	// Remove common suffixes
	title = strings.Replace(title, " – Download", "", -1)
	title = strings.Replace(title, " - Download", "", -1)
	title = strings.TrimSpace(title)

	// Add season if present
	if season != "" {
		title = fmt.Sprintf("%s S%s - %sª Temporada", title, fmt.Sprintf("%02s", season), season)
	}

	// Add audio ISO 639-2 code to title between ()
	title = appendAudioISO639_2Code(title, audio)

	return title
}
