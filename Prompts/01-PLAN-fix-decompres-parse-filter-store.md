# Oprava Wiki parseru — diagnostika zaseknutí na `prošlo_stránek=4`

## Kontext

`make download-philosophy-wiki` visí při parsování CS Wikipedia dumpu (1.27 GB) na `prošlo_stránek=4`. Uživatel správně identifikoval, že **SQLite deadlock NENÍ příčina** — counter `scanned` se inkrementuje PŘED zápisem do DB (viz `internal/wiki/downloader.go:211`), takže při `nalezeno=0` neproběhl žádný DB zápis. Problém je mezi bzip2 dekompresí a XML parserem.

**Cíl:** rozchodit CS wiki parsing a zároveň přidat diagnostiku, abychom viděli, která vrstva je skutečný bottleneck.

## Analýza současného stavu

### Co už funguje dobře
- **SQLite** v `internal/storage/db.go:42-69` — DELETE journal mode (vyřešený shm mutex deadlock na macOS), per-PRAGMA `Exec()`, `SetMaxOpenConns(1)`, `SaveDocumentBatch` s `context.WithTimeout` a prepared statements
- **Downloader** má retry/resume přes HTTP Range, `.done` marker, per-language cache file
- **Callback architektura** `Download(fn func(*Page) error) error` — eliminovala backpressure deadlocky z channel pipeline
- **progressReader** struktura už existuje v `downloader.go:253` — můžeme ji znovu použít pro byte counter

### Co je podezřelé
- `encoding/xml` + `decoder.Token()` + `DecodeElement()` na 6 GB uncompressed Wikipedia XML je notoricky pomalý (Go's pure-Go XML parser zvládá ~10-30 MB/s)
- V `downloader.go:179-242` je **mrtvý debug kód** (`titlePreview` smyčka, `slog.Debug` timing každého kroku) — masivně zpomaluje parser při `--log debug`
- Žádný byte-level progress → nevidíme jestli je zaseklý bzip2 nebo XML parser
- Žádný `bufio.Reader` okolo pipe → malé čtení, mnoho syscallů

### Diagnostika prostředí
- `bzip2` 1.0.8 dostupný v `/usr/bin/bzip2` — benchmark: ~45 MB/s single-thread
- `lbzip2` uživatel právě doinstaloval (deprecated v brew 2025-07-07, ale funkční; umí paralelizovat single-stream bzip2 přes block header scanning)
- `pbzip2` nedostupný (a stejně by nepomohl — single-stream nepodporuje)

## Navrhovaná oprava

### 1. `internal/wiki/downloader.go` — přepsat Fázi 2 parsing

**Cíl:** rychlý streamový parser + měřitelná diagnostika na všech vrstvách.

#### 1a. Dekomprese: `lbzip2` s fallbackem na `bzip2`

```go
// Detekce dostupnosti lbzip2 (rychlejší, ale deprecated)
var bzCmd *exec.Cmd
if path, err := exec.LookPath("lbzip2"); err == nil {
    slog.Info("Using lbzip2 for decompression", "path", path)
    bzCmd = exec.Command("lbzip2", "-d", "-c", cacheFile)
} else {
    slog.Info("Using standard bzip2 for decompression")
    bzCmd = exec.Command("bzip2", "-d", "-c", cacheFile)
}
bzCmd.Stderr = os.Stderr
bzOut, err := bzCmd.StdoutPipe()
if err != nil { return fmt.Errorf("bzip2 pipe: %w", err) }
if err := bzCmd.Start(); err != nil { return fmt.Errorf("bzip2 start: %w", err) }
defer func() { bzOut.Close(); bzCmd.Wait() }()
```

#### 1b. Byte-level progress tracking

Obal `bzOut` do existujícího `progressReader` (už v souboru na řádku 253), potom do `bufio.NewReaderSize(r, 4<<20)` (4 MB buffer).

```go
var bytesRead int64
prReader := &progressReader{r: bzOut, n: &bytesRead}
bufReader := bufio.NewReaderSize(prReader, 4<<20)
```

Ticker loguje každých 10 s **tři metriky**:
- `rozbaleno_MB` — byte counter (rychlost dekomprese)
- `prošlo_stránek` — NS=0 stránky (rychlost XML parseru)
- `nalezeno` — matched pages (rychlost DB zápisu)

Diagnostika:
- Pokud `rozbaleno_MB` stojí → bzip2/pipe zaseknutý
- Pokud `rozbaleno_MB` roste, ale `prošlo_stránek` ne → XML parser je bottleneck
- Pokud obě rostou, ale `nalezeno` ne → filter nebo DB

#### 1c. Rychlý parser: nahradit `encoding/xml` line-based scannerem

Wikipedia XML dump má předvídatelnou strukturu — každý element na vlastním řádku. Ručně skenovat `<page>...</page>` bloky a extrahovat pole přes `bytes.Index` je **o 5-20× rychlejší** než `encoding/xml`:

```go
// Scanner přes <page>...</page> bloky
var pageBuf bytes.Buffer
inPage := false
pageStart := []byte("<page>")
pageEnd := []byte("</page>")

for {
    line, err := bufReader.ReadBytes('\n')
    if len(line) > 0 {
        if !inPage {
            if idx := bytes.Index(line, pageStart); idx >= 0 {
                inPage = true
                pageBuf.Reset()
                pageBuf.Write(line[idx:])
                if bytes.Contains(line[idx:], pageEnd) {
                    inPage = false
                    if perr := d.processPageBlock(pageBuf.Bytes(), fn, &scanned, &count, &skipped); perr != nil { return perr }
                    atomic.StoreInt64(&scannedAtomic, int64(scanned))
                    atomic.StoreInt64(&countAtomic, int64(count))
                    if d.cfg.MaxPages > 0 && count >= d.cfg.MaxPages { break }
                }
            }
        } else {
            pageBuf.Write(line)
            if bytes.Contains(line, pageEnd) {
                inPage = false
                if perr := d.processPageBlock(pageBuf.Bytes(), fn, &scanned, &count, &skipped); perr != nil { return perr }
                atomic.StoreInt64(&scannedAtomic, int64(scanned))
                atomic.StoreInt64(&countAtomic, int64(count))
                if d.cfg.MaxPages > 0 && count >= d.cfg.MaxPages { break }
            }
        }
    }
    if err == io.EOF { break }
    if err != nil { return fmt.Errorf("read bzip2: %w", err) }
}
```

#### 1d. Extrakční helpery

```go
// processPageBlock parsuje jeden <page>...</page> blok.
func (d *Downloader) processPageBlock(data []byte, fn func(*Page) error, scanned, count, skipped *int) error {
    // Rychlé filtry (redirect, NS != 0) — nejdřív, abychom nečetli text zbytečně
    if bytes.Contains(data, []byte("<redirect ")) { return nil }
    nsBytes := extractBetween(data, []byte("<ns>"), []byte("</ns>"))
    if ns, _ := strconv.Atoi(string(nsBytes)); ns != 0 { return nil }

    title := html.UnescapeString(string(extractBetween(data, []byte("<title>"), []byte("</title>"))))
    rawText := extractText(data) // speciální, <text> má atributy

    *scanned++
    if !d.matchesTopic(title, rawText) { *skipped++; return nil }

    text := cleanWikitext(html.UnescapeString(rawText))
    if len(text) < 100 { return nil }

    if err := fn(&Page{Title: title, Text: text, Lang: d.cfg.Lang}); err != nil { return err }
    *count++
    return nil
}

// extractBetween vrátí obsah mezi open a close tagem (první výskyt, bez alokace stringu).
func extractBetween(data, open, close []byte) []byte {
    s := bytes.Index(data, open)
    if s < 0 { return nil }
    s += len(open)
    e := bytes.Index(data[s:], close)
    if e < 0 { return nil }
    return data[s : s+e]
}

// extractText řeší <text xml:space="preserve" bytes="..."> otevírací tag s atributy.
func extractText(data []byte) string {
    s := bytes.Index(data, []byte("<text"))
    if s < 0 { return "" }
    s += 5
    gt := bytes.IndexByte(data[s:], '>')
    if gt < 0 { return "" }
    s += gt + 1
    e := bytes.Index(data[s:], []byte("</text>"))
    if e < 0 { return "" }
    return string(data[s : s+e])
}
```

#### 1e. Cleanup

- Smazat `xmlPage` strukturu (`downloader.go:86-96`)
- Smazat import `encoding/xml`
- Přidat importy: `bufio`, `bytes`, `html`, `strconv`
- Smazat debug balast `downloader.go:193-200` (`titlePreview` smyčka) a `downloader.go:215-234` (`slog.Debug` timing každého kroku)

### 2. `cmd/downloader/main.go` — větší DB batch (volitelné, až bude parsing rozchozený)

Aktuálně `runWiki` posílá do `SaveDocumentBatch` po jedné stránce (1-N chunků). Pro lepší DB throughput akumulovat ~1000 dokumentů:

```go
var pending []*storage.Document
err := downloader.Download(func(page *wiki.Page) error {
    // ... transform page na chunks ...
    pending = append(pending, docs...)
    if len(pending) >= 1000 {
        n, _ := db.SaveDocumentBatch(pending)
        saved += n
        pending = pending[:0]
    }
    return nil
})
// Flush rest
if len(pending) > 0 {
    n, _ := db.SaveDocumentBatch(pending)
    saved += n
}
```

**Pozn:** neřeší hlavní symptom (stuck at 4), nechat až bude primární oprava ověřená.

### 3. `internal/storage/db.go` — přidat `PRAGMA temp_store=MEMORY` (kosmetika)

Jeden řádek do smyčky v `Open()` — zrychlí temp tabulky a indexy v paměti. **Neměnit DELETE journal mode** (úmyslné kvůli macOS shm mutex bug).

## Kritické soubory

| Soubor | Změna | Řádky |
|--------|-------|-------|
| `internal/wiki/downloader.go` | Přepsat Fázi 2 parsing, smazat xmlPage, debug cruft, přidat helpery | 6-18 (importy), 86-96 (smazat), 139-242 (přepsat), nové helpery na konec |
| `internal/storage/db.go` | `PRAGMA temp_store=MEMORY` | 53-58 |
| `cmd/downloader/main.go` | (volitelné) batch akumulace | 118-151 |

## Verifikace

### Krok 1: Build
```bash
go build ./...
```

### Krok 2: Test s malým limitem (měl by prokázat že se parser pohybuje)
```bash
go run cmd/downloader/main.go \
  --source wiki --lang cs --max-pages 10 \
  --topics "filozofie,stoicismus,seneca" \
  --data-dir ./data \
  --log info 2>&1
```

**Očekávané chování:**
- `Using lbzip2 for decompression` (nebo bzip2 fallback)
- `Using cached dump file=data/raw/wiki_dump_cs.xml.bz2`
- Každých 10 s: `rozbaleno_MB=XXX prošlo_stránek=YYY nalezeno=Z elapsed=Ns`
- `rozbaleno_MB` rychle roste (desítky až stovky MB/10s)
- `prošlo_stránek` roste v řádu stovek/tisíců za ticker
- `nalezeno=10` do ~1-3 minut, parser se zastaví

### Krok 3: Pokud se něco zasekne, diagnostika přes counters:
- `rozbaleno_MB` roste, `prošlo_stránek` 0 → parser bloken na něčem specifickém v XML (přidat debug print raw bytů blízko zásek)
- `rozbaleno_MB` nula → bzip2/pipe problém (zkusit fallback na standard `bzip2`)
- Obojí roste, `nalezeno` 0 → matchesTopic filter nic nenachází (rozšířit topics nebo debug logging pro každý scanned title)

### Krok 4: Pokud jede, spustit plný make target
```bash
make download-philosophy-wiki
```

### Krok 5: Ověřit obsah DB
```bash
sqlite3 data/rag_edu.db "SELECT COUNT(*), source FROM documents GROUP BY source;"
sqlite3 data/rag_edu.db "SELECT title FROM documents WHERE source='wikipedia' LIMIT 20;"
```
