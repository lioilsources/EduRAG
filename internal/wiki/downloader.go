// internal/wiki/downloader.go
// Stahuje a parsuje Wikipedia XML dumpy.
// Simple English Wikipedia (~250MB) je ideální pro 1. stupeň ZŠ.
package wiki

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// Article jeden článek z Wikipedia XML dumpu — raw wikitext, před cleaningem.
// Filter a cleaning se dělají až v pozdějších fázích (cmd/wiki-filter).
type Article struct {
	ID      int64  // Wikipedia page ID (kanonický klíč)
	Title   string
	RawText string // nezpracovaný wikitext
	Lang    string
}

// xmlPage interní struktura pro encoding/xml decoder.
// Jen pole, která skutečně potřebujeme — id, ns filter, redirect detekce, text revize.
type xmlPage struct {
	ID       int64        `xml:"id"`
	Title    string       `xml:"title"`
	NS       int          `xml:"ns"`
	Redirect *xmlRedirect `xml:"redirect"`
	Revision xmlRevision  `xml:"revision"`
}

type xmlRedirect struct {
	Title string `xml:"title,attr"`
}

type xmlRevision struct {
	Text xmlText `xml:"text"`
}

type xmlText struct {
	Body string `xml:",chardata"`
}

// DownloadConfig konfigurace stahování.
// Topic filtering bylo odstraněno — downloader ukládá VŠECHNY NS=0 non-redirect články
// do wiki_<lang>.db a filtrování se dělá až v cmd/wiki-filter přes SQL.
type DownloadConfig struct {
	URL       string
	OutputDir string
	MaxPages  int    // 0 = bez limitu
	Lang      string // "en", "simple", "cs"
}

// Downloader stahuje a parsuje Wikipedia dump.
type Downloader struct {
	cfg    DownloadConfig
	client *http.Client
}

// NewDownloader vytvoří nový downloader.
func NewDownloader(cfg DownloadConfig) *Downloader {
	return &Downloader{
		cfg: cfg,
		client: &http.Client{
			Timeout: 0, // bez timeoutu — dumpy jsou velké
		},
	}
}

// Download stáhne dump (pokud není cached) a synchronně parsuje články.
// Pro každý NS=0 non-redirect článek zavolá fn s raw wikitext. Žádný filtr,
// žádný cleaning — to dělá cmd/wiki-filter v další fázi pipeline.
// Pokud fn vrátí error, parsování se zastaví.
func (d *Downloader) Download(fn func(*Article) error) error {
	lang := d.cfg.Lang
	if lang == "" {
		lang = "unknown"
	}
	cacheFile := filepath.Join(d.cfg.OutputDir, "wiki_dump_"+lang+".xml.bz2")
	if err := os.MkdirAll(d.cfg.OutputDir, 0755); err != nil {
		return err
	}

	// Fáze 1: stáhni celý soubor (s retry a resume)
	doneMarker := cacheFile + ".done"
	if _, err := os.Stat(doneMarker); os.IsNotExist(err) {
		const maxRetries = 10
		var dlErr error
		for attempt := 1; attempt <= maxRetries; attempt++ {
			dlErr = d.downloadFile(cacheFile)
			if dlErr == nil {
				break
			}
			if attempt < maxRetries {
				wait := time.Duration(attempt*attempt) * 5 * time.Second
				slog.Warn("Download přerušen, zkouším znovu",
					"pokus", fmt.Sprintf("%d/%d", attempt, maxRetries),
					"chyba", dlErr,
					"čekám", wait,
				)
				time.Sleep(wait)
			}
		}
		if dlErr != nil {
			return dlErr
		}
		os.WriteFile(doneMarker, []byte("ok"), 0644)
	} else {
		slog.Info("Using cached dump", "file", cacheFile)
	}

	// Fáze 2: dekomprese + streamový XML parser.
	// lbzip2 je výrazně rychlejší (paralelní single-stream dekomprese), fallback na standard bzip2.
	var bzCmd *exec.Cmd
	if lbzPath, err := exec.LookPath("lbzip2"); err == nil {
		slog.Info("Using lbzip2 for decompression", "path", lbzPath)
		bzCmd = exec.Command("lbzip2", "-d", "-c", cacheFile)
	} else {
		slog.Info("Using standard bzip2 for decompression (install lbzip2 for faster parallel decode)")
		bzCmd = exec.Command("bzip2", "-d", "-c", cacheFile)
	}
	bzCmd.Stderr = os.Stderr
	bzOut, err := bzCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("bzip2 pipe: %w", err)
	}
	if err := bzCmd.Start(); err != nil {
		return fmt.Errorf("bzip2 start: %w", err)
	}
	defer func() {
		bzOut.Close()
		bzCmd.Wait()
	}()

	// Byte counter — diagnostika jestli bzip2 skutečně streamuje.
	var bytesRead int64
	prReader := &progressReader{r: bzOut, n: &bytesRead}

	// sanitizeReader nahradí HTML entity (&nbsp; &mdash; …) a zahodí
	// neplatné XML 1.0 control znaky — Go encoding/xml zná jen standardní
	// XML entity a CS Wikipedia dump obsahuje desítky HTML-only entit,
	// na kterých by se parser zasekl.
	cleanReader := newSanitizeReader(prReader)

	decoder := xml.NewDecoder(cleanReader)
	decoder.Strict = false

	count := 0
	scanned := 0
	start := time.Now()

	// Progress ticker — tři metriky pro diagnostiku vrstev.
	//   rozbaleno_MB   — rychlost dekomprese (bzip2/lbzip2 → pipe → bytesRead)
	//   prošlo_stránek — rychlost XML parseru (včetně redirectů a ne-článků)
	//   nalezeno       — počet článků odeslaných do callback fn (NS=0, non-redirect)
	var scannedAtomic, countAtomic int64
	stopTicker := make(chan struct{})
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				slog.Info("Parsování Wiki",
					"rozbaleno_MB", fmt.Sprintf("%.1f", float64(atomic.LoadInt64(&bytesRead))/1e6),
					"prošlo_stránek", atomic.LoadInt64(&scannedAtomic),
					"nalezeno", atomic.LoadInt64(&countAtomic),
					"elapsed", time.Since(start).Round(time.Second),
				)
			case <-stopTicker:
				return
			}
		}
	}()
	defer close(stopTicker)

parseLoop:
	for {
		tok, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("xml token: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "page" {
			continue
		}

		var xp xmlPage
		if err := decoder.DecodeElement(&xp, &se); err != nil {
			slog.Warn("decode page failed", "err", err)
			continue
		}
		scanned++
		atomic.StoreInt64(&scannedAtomic, int64(scanned))

		// Přeskoč redirecty a ne-článkové namespace (talk, user, šablony, …).
		if xp.Redirect != nil || xp.NS != 0 {
			continue
		}

		if err := fn(&Article{
			ID:      xp.ID,
			Title:   xp.Title,
			RawText: xp.Revision.Text.Body,
			Lang:    d.cfg.Lang,
		}); err != nil {
			return err
		}
		count++
		atomic.StoreInt64(&countAtomic, int64(count))

		if d.cfg.MaxPages > 0 && count >= d.cfg.MaxPages {
			slog.Info("Reached max pages limit", "limit", d.cfg.MaxPages)
			break parseLoop
		}
	}

	slog.Info("Wiki parsování hotovo",
		"rozbaleno_MB", fmt.Sprintf("%.1f", float64(atomic.LoadInt64(&bytesRead))/1e6),
		"prošlo_stránek", scanned,
		"nalezeno", count,
		"elapsed", time.Since(start).Round(time.Second),
	)
	return nil
}

// progressReader obaluje io.Reader a počítá přečtené bajty.
type progressReader struct {
	r io.Reader
	n *int64
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	atomic.AddInt64(pr.n, int64(n))
	return n, err
}

// sanitizeReader obaluje io.Reader a PŘED XML parserem:
//   - nahrazuje nestandardní HTML entity (&nbsp;, &mdash;, …) za UTF-8 ekvivalenty
//   - zahazuje neplatné XML 1.0 control znaky (kromě \t \n \r)
//
// Go encoding/xml zná jen 5 standardních XML entit (&amp; &lt; &gt; &apos; &quot;).
// CS Wikipedia dump obsahuje desítky HTML-only entit — bez tohoto kroku parser
// buď spadne, nebo (při Strict=false) tiše vyhodí element a ztratí stránky.
//
// Strategie: udržujeme `tail` buffer s posledními ~16 bajty dat, abychom
// neřezali entitu uprostřed mezi voláními Read.
type sanitizeReader struct {
	r    io.Reader
	tail []byte // carry-over — možný začátek entity přes boundary
	out  []byte // připravený sanitizovaný výstup
}

const maxEntityLen = 16 // nejdelší entita v tabulce má ~8 znaků, 16 = bezpečná rezerva

func newSanitizeReader(r io.Reader) *sanitizeReader {
	return &sanitizeReader{r: r}
}

func (sr *sanitizeReader) Read(p []byte) (int, error) {
	// Dodej zbytek z předchozího volání
	if len(sr.out) > 0 {
		n := copy(p, sr.out)
		sr.out = sr.out[n:]
		return n, nil
	}

	buf := make([]byte, len(p)*2)
	n, err := sr.r.Read(buf)
	if n == 0 {
		if len(sr.tail) > 0 && err != nil {
			// EOF — flush zbytek
			sr.out = sanitize(sr.tail)
			sr.tail = nil
			if len(sr.out) == 0 {
				return 0, err
			}
			m := copy(p, sr.out)
			sr.out = sr.out[m:]
			return m, nil
		}
		return 0, err
	}

	// Připoj carry-over z minula
	var combined []byte
	if len(sr.tail) > 0 {
		combined = make([]byte, 0, len(sr.tail)+n)
		combined = append(combined, sr.tail...)
		combined = append(combined, buf[:n]...)
		sr.tail = nil
	} else {
		combined = buf[:n]
	}

	// Kromě EOF si nech posledních maxEntityLen bajtů jako nový tail,
	// aby entita rozdělená přes boundary nebyla chybně sanitizována.
	var toSanitize []byte
	if err == nil && len(combined) > maxEntityLen {
		split := len(combined) - maxEntityLen
		// Pokud je v hvostu '&', posuň split před něj (nevíme kde entita končí).
		if amp := bytes.LastIndexByte(combined[split:], '&'); amp >= 0 {
			split += amp
		}
		toSanitize = combined[:split]
		sr.tail = append([]byte(nil), combined[split:]...)
	} else {
		toSanitize = combined
	}

	sr.out = sanitize(toSanitize)
	if len(sr.out) == 0 {
		// Vše padlo do tailu nebo bylo filtrováno — rekurzivně načti víc
		return sr.Read(p)
	}
	m := copy(p, sr.out)
	sr.out = sr.out[m:]
	return m, err
}

// htmlEntities — nejčastější HTML entity ve Wikipedia článcích.
// Překládáme na UTF-8 znaky; vše ostatní (neznámé entity) sanitize nechá projít.
var htmlEntities = []struct{ from, to string }{
	{"&nbsp;", "\u00a0"}, {"&mdash;", "\u2014"}, {"&ndash;", "\u2013"},
	{"&hellip;", "\u2026"}, {"&laquo;", "\u00ab"}, {"&raquo;", "\u00bb"},
	{"&ldquo;", "\u201c"}, {"&rdquo;", "\u201d"}, {"&lsquo;", "\u2018"},
	{"&rsquo;", "\u2019"}, {"&bull;", "\u2022"}, {"&middot;", "\u00b7"},
	{"&times;", "\u00d7"}, {"&divide;", "\u00f7"}, {"&minus;", "\u2212"},
	{"&plusmn;", "\u00b1"}, {"&deg;", "\u00b0"}, {"&micro;", "\u00b5"},
	{"&para;", "\u00b6"}, {"&copy;", "\u00a9"}, {"&reg;", "\u00ae"},
	{"&trade;", "\u2122"}, {"&euro;", "\u20ac"}, {"&pound;", "\u00a3"},
	{"&yen;", "\u00a5"}, {"&cent;", "\u00a2"}, {"&acute;", "\u00b4"},
	{"&uml;", "\u00a8"}, {"&cedil;", "\u00b8"}, {"&iquest;", "\u00bf"},
	{"&iexcl;", "\u00a1"}, {"&szlig;", "\u00df"}, {"&oslash;", "\u00f8"},
	{"&Oslash;", "\u00d8"}, {"&agrave;", "\u00e0"}, {"&aacute;", "\u00e1"},
	{"&eacute;", "\u00e9"}, {"&egrave;", "\u00e8"}, {"&iacute;", "\u00ed"},
	{"&oacute;", "\u00f3"}, {"&uacute;", "\u00fa"}, {"&ntilde;", "\u00f1"},
	{"&Ntilde;", "\u00d1"}, {"&ccedil;", "\u00e7"}, {"&Ccedil;", "\u00c7"},
	{"&thinsp;", "\u2009"}, {"&ensp;", "\u2002"}, {"&emsp;", "\u2003"},
	{"&zwnj;", "\u200c"}, {"&zwj;", "\u200d"}, {"&lrm;", "\u200e"},
	{"&rlm;", "\u200f"}, {"&sbquo;", "\u201a"}, {"&bdquo;", "\u201e"},
	{"&dagger;", "\u2020"}, {"&Dagger;", "\u2021"}, {"&permil;", "\u2030"},
	{"&prime;", "\u2032"}, {"&Prime;", "\u2033"}, {"&lsaquo;", "\u2039"},
	{"&rsaquo;", "\u203a"}, {"&oline;", "\u203e"}, {"&frasl;", "\u2044"},
	{"&image;", "\u2111"}, {"&weierp;", "\u2118"}, {"&real;", "\u211c"},
	{"&alefsym;", "\u2135"}, {"&larr;", "\u2190"}, {"&uarr;", "\u2191"},
	{"&rarr;", "\u2192"}, {"&darr;", "\u2193"}, {"&harr;", "\u2194"},
	{"&crarr;", "\u21b5"}, {"&sub;", "\u2282"}, {"&sup;", "\u2283"},
	{"&sim;", "\u223c"}, {"&cong;", "\u2245"}, {"&asymp;", "\u2248"},
	{"&ne;", "\u2260"}, {"&equiv;", "\u2261"}, {"&le;", "\u2264"},
	{"&ge;", "\u2265"}, {"&there4;", "\u2234"}, {"&infin;", "\u221e"},
	{"&empty;", "\u2205"}, {"&nabla;", "\u2207"}, {"&isin;", "\u2208"},
	{"&notin;", "\u2209"}, {"&ni;", "\u220b"}, {"&prod;", "\u220f"},
	{"&sum;", "\u2211"}, {"&prop;", "\u221d"}, {"&ang;", "\u2220"},
	{"&and;", "\u2227"}, {"&or;", "\u2228"}, {"&cap;", "\u2229"},
	{"&cup;", "\u222a"}, {"&int;", "\u222b"}, {"&sdot;", "\u22c5"},
	{"&oplus;", "\u2295"}, {"&otimes;", "\u2297"}, {"&perp;", "\u22a5"},
	{"&Alpha;", "\u0391"}, {"&Beta;", "\u0392"}, {"&Gamma;", "\u0393"},
	{"&spades;", "\u2660"}, {"&clubs;", "\u2663"}, {"&hearts;", "\u2665"},
	{"&diams;", "\u2666"},
}

func sanitize(data []byte) []byte {
	s := string(data)
	for _, e := range htmlEntities {
		if strings.Contains(s, e.from) {
			s = strings.ReplaceAll(s, e.from, e.to)
		}
	}
	// Odstraň neplatné XML 1.0 control characters (kromě \t \n \r).
	var b bytes.Buffer
	b.Grow(len(s))
	for _, r := range s {
		if r == 0x09 || r == 0x0A || r == 0x0D || (r >= 0x20 && r != 0xFFFE && r != 0xFFFF) {
			b.WriteRune(r)
		}
	}
	return b.Bytes()
}

// downloadFile stáhne dump do souboru s podporou resume (HTTP Range).
// Pokud existuje nekompletní soubor, pokračuje od místa přerušení.
// Vrací pouze error — caller zapisuje do errs kanálu.
func (d *Downloader) downloadFile(dest string) error {
	// Zjisti velikost existujícího fragmentu pro resume
	var offset int64
	if fi, err := os.Stat(dest); err == nil {
		offset = fi.Size()
		slog.Info("Resuming download", "soubor", dest, "offset", fmt.Sprintf("%.1f MB", float64(offset)/1e6))
	}

	req, err := http.NewRequest("GET", d.cfg.URL, nil)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	// Server nemusí podporovat Range — pak stahuj znovu od začátku
	if offset > 0 && resp.StatusCode == 200 {
		slog.Warn("Server nepodporuje Range, stahuje se od začátku")
		offset = 0
	} else if resp.StatusCode != 200 && resp.StatusCode != 206 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	flags := os.O_CREATE | os.O_WRONLY
	if offset > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(dest, flags, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	// Celková velikost = offset + zbývající content-length
	totalBytes := resp.ContentLength
	if totalBytes > 0 {
		totalBytes += offset
	}

	var downloaded int64
	atomic.StoreInt64(&downloaded, offset)
	pr := &progressReader{r: resp.Body, n: &downloaded}

	const stallTimeout = 60 * time.Second
	stallCancel := make(chan struct{})
	stallErr := make(chan error, 1)

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		lastSeen := atomic.LoadInt64(&downloaded)
		stalledFor := time.Duration(0)

		for {
			select {
			case <-ticker.C:
				n := atomic.LoadInt64(&downloaded)
				if totalBytes > 0 {
					pct := float64(n) / float64(totalBytes) * 100
					slog.Info("Stahování dumpu",
						"staženo", fmt.Sprintf("%.1f MB", float64(n)/1e6),
						"celkem", fmt.Sprintf("%.1f MB", float64(totalBytes)/1e6),
						"procent", fmt.Sprintf("%.0f%%", pct),
					)
				} else {
					slog.Info("Stahování dumpu", "staženo", fmt.Sprintf("%.1f MB", float64(n)/1e6))
				}
				// Detekuj stall — žádný nový bajt po dobu stallTimeout
				if n == lastSeen {
					stalledFor += 5 * time.Second
					if stalledFor >= stallTimeout {
						slog.Warn("Download stall — žádná data po 60s, restartuji")
						stallErr <- fmt.Errorf("download stalled: no data for %s", stallTimeout)
						resp.Body.Close()
						return
					}
				} else {
					stalledFor = 0
					lastSeen = n
				}
			case <-stallCancel:
				return
			}
		}
	}()
	defer close(stallCancel)

	copyErr := make(chan error, 1)
	go func() {
		_, err := io.Copy(f, pr)
		copyErr <- err
	}()

	select {
	case err := <-copyErr:
		if err != nil {
			return fmt.Errorf("download write: %w", err)
		}
	case err := <-stallErr:
		return err
	}

	n := atomic.LoadInt64(&downloaded)
	slog.Info("Download dokončen", "soubor", dest, "velikost", fmt.Sprintf("%.1f MB", float64(n)/1e6))
	return nil
}

// CleanWikitext odstraní základní wiki markup (šablony, linky, HTML tagy, heading markery).
// Pro produkci doporučit pandoc nebo go-wikiparser.
func CleanWikitext(text string) string {
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
