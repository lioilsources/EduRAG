// internal/wiki/downloader.go
// Stahuje a parsuje Wikipedia XML dumpy.
// Simple English Wikipedia (~250MB) je ideální pro 1. stupeň ZŠ.
package wiki

import (
	"compress/bzip2"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Dump URL šablony
const (
	// Simple English Wikipedia — kratší, pedagogičtější texty
	SimpleEnDumpURL = "https://dumps.wikimedia.org/simplewiki/latest/simplewiki-latest-pages-articles.xml.bz2"
	// Česká Wikipedia
	CsDumpURL = "https://dumps.wikimedia.org/cswiki/latest/cswiki-latest-pages-articles.xml.bz2"
	// Anglická Wikipedia (velká — doporučit jen pro specifická témata)
	EnDumpURL = "https://dumps.wikimedia.org/enwiki/latest/enwiki-latest-pages-articles.xml.bz2"
)

// Page jeden článek z Wikipedia XML dumpu.
type Page struct {
	Title string
	Text  string
	Lang  string
}

// TopicFilter filtruje stránky podle relevantních témat.
// Pro 1. stupeň ZŠ — přírodověda, dějepis, zeměpis, zahrádka, filozofie.
var DefaultTopics = []string{
	// Přírodověda
	"plant", "animal", "biology", "ecology", "photosynthesis",
	"weather", "climate", "water", "soil", "seed", "flower",
	// Dějepis
	"history", "ancient", "medieval", "war", "civilization",
	"egypt", "rome", "greece", "castle", "knight",
	// Zeměpis
	"country", "continent", "river", "mountain", "ocean",
	"europe", "africa", "asia", "america",
	// Chemie / fyzika základy
	"element", "atom", "chemical", "material", "metal",
	// Zahrádka
	"garden", "vegetable", "fruit", "herb", "compost", "bee",
	// Filozofie / umění
	"philosophy", "art", "painting", "sculpture", "music",
	"democracy", "justice", "ancient greece",
}

// DownloadConfig konfigurace stahování.
type DownloadConfig struct {
	URL        string
	OutputDir  string
	MaxPages   int      // 0 = bez limitu
	Topics     []string // filtrovací klíčová slova (lowercase)
	Lang       string   // "en", "simple", "cs"
}

// Downloader stahuje a parsuje Wikipedia dump.
type Downloader struct {
	cfg    DownloadConfig
	client *http.Client
}

// NewDownloader vytvoří nový downloader.
func NewDownloader(cfg DownloadConfig) *Downloader {
	if len(cfg.Topics) == 0 {
		cfg.Topics = DefaultTopics
	}
	return &Downloader{
		cfg: cfg,
		client: &http.Client{
			Timeout: 0, // bez timeoutu — dumpy jsou velké
		},
	}
}

// xmlPage interní struktura pro XML parsing.
type xmlPage struct {
	Title    string `xml:"title"`
	Redirect struct {
		Title string `xml:"title,attr"`
	} `xml:"redirect"`
	Revision struct {
		Text string `xml:"text"`
	} `xml:"revision"`
	NS int `xml:"ns"`
}

// Download stáhne dump a streamově parsuje stránky.
// Vrací kanál s Page objekty — zpracovávej asynchronně.
func (d *Downloader) Download() (<-chan *Page, <-chan error) {
	pages := make(chan *Page, 100)
	errs := make(chan error, 1)

	go func() {
		defer close(pages)
		defer close(errs)

		// Zkontroluj zda soubor existuje lokálně (cache)
		cacheFile := filepath.Join(d.cfg.OutputDir, "wiki_dump.xml.bz2")
		if err := os.MkdirAll(d.cfg.OutputDir, 0755); err != nil {
			errs <- err
			return
		}

		var reader io.Reader

		if _, err := os.Stat(cacheFile); os.IsNotExist(err) {
			slog.Info("Downloading Wikipedia dump", "url", d.cfg.URL)
			resp, err := d.client.Get(d.cfg.URL)
			if err != nil {
				errs <- fmt.Errorf("download: %w", err)
				return
			}
			defer resp.Body.Close()

			// Uložit a zároveň číst (tee)
			f, err := os.Create(cacheFile)
			if err != nil {
				errs <- err
				return
			}
			defer f.Close()

			reader = io.TeeReader(resp.Body, f)
		} else {
			slog.Info("Using cached dump", "file", cacheFile)
			f, err := os.Open(cacheFile)
			if err != nil {
				errs <- err
				return
			}
			defer f.Close()
			reader = f
		}

		bzReader := bzip2.NewReader(reader)
		decoder := xml.NewDecoder(bzReader)

		count := 0
		skipped := 0
		start := time.Now()

		for {
			token, err := decoder.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				errs <- fmt.Errorf("xml decode: %w", err)
				return
			}

			se, ok := token.(xml.StartElement)
			if !ok || se.Name.Local != "page" {
				continue
			}

			var p xmlPage
			if err := decoder.DecodeElement(&p, &se); err != nil {
				continue
			}

			// Přeskočit přesměrování a non-article NS
			if p.NS != 0 || p.Redirect.Title != "" {
				continue
			}

			// Filtrovat podle témat
			if !d.matchesTopic(p.Title, p.Revision.Text) {
				skipped++
				continue
			}

			text := cleanWikitext(p.Revision.Text)
			if len(text) < 100 {
				continue
			}

			page := &Page{
				Title: p.Title,
				Text:  text,
				Lang:  d.cfg.Lang,
			}

			pages <- page
			count++

			if count%1000 == 0 {
				slog.Info("Wiki progress",
					"pages", count,
					"skipped", skipped,
					"elapsed", time.Since(start).Round(time.Second),
				)
			}

			if d.cfg.MaxPages > 0 && count >= d.cfg.MaxPages {
				slog.Info("Reached max pages limit", "limit", d.cfg.MaxPages)
				break
			}
		}

		slog.Info("Wiki download complete",
			"total_pages", count,
			"skipped", skipped,
			"elapsed", time.Since(start).Round(time.Second),
		)
	}()

	return pages, errs
}

// matchesTopic vrátí true pokud titulek nebo začátek textu odpovídá tématu.
func (d *Downloader) matchesTopic(title, text string) bool {
	titleLower := strings.ToLower(title)
	// Prvních 500 znaků textu — rychlý check
	preview := strings.ToLower(text)
	if len(preview) > 500 {
		preview = preview[:500]
	}

	for _, topic := range d.cfg.Topics {
		if strings.Contains(titleLower, topic) {
			return true
		}
		if strings.Contains(preview, topic) {
			return true
		}
	}
	return false
}

// cleanWikitext odstraní základní wiki markup.
// Pro produkci doporučit pandoc nebo go-wikiparser.
func cleanWikitext(text string) string {
	var sb strings.Builder
	sb.Grow(len(text))

	inTemplate := 0
	inLink := 0
	i := 0

	for i < len(text) {
		// Přeskočit šablony {{ ... }}
		if i+1 < len(text) && text[i] == '{' && text[i+1] == '{' {
			inTemplate++
			i += 2
			continue
		}
		if i+1 < len(text) && text[i] == '}' && text[i+1] == '}' {
			if inTemplate > 0 {
				inTemplate--
			}
			i += 2
			continue
		}
		if inTemplate > 0 {
			i++
			continue
		}

		// Zpracovat [[links]]
		if i+1 < len(text) && text[i] == '[' && text[i+1] == '[' {
			inLink++
			i += 2
			continue
		}
		if i+1 < len(text) && text[i] == ']' && text[i+1] == ']' {
			if inLink > 0 {
				inLink--
			}
			i += 2
			continue
		}

		// Přeskočit category/file links
		if inLink > 0 {
			// Hledej text za | nebo přeskoč celý link
			if text[i] == '|' {
				// Zbytek linku je display text — přidej ho
				i++
				continue
			}
		}

		// Odstranit HTML tagy
		if text[i] == '<' {
			end := strings.Index(text[i:], ">")
			if end != -1 {
				i += end + 1
				continue
			}
		}

		// Přeskočit nadpisy markery ale zachovat text
		if text[i] == '=' {
			sb.WriteByte('\n')
			for i < len(text) && text[i] == '=' {
				i++
			}
			continue
		}

		sb.WriteByte(text[i])
		i++
	}

	// Vyčistit prázdné řádky a whitespace
	result := sb.String()
	lines := strings.Split(result, "\n")
	var clean []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "*") && !strings.HasPrefix(line, "#") {
			clean = append(clean, line)
		}
	}

	return strings.Join(clean, "\n")
}
