# Wikipedia import — finální oprava a dvoufázová architektura

## Co bylo špatně (shrnutí debugování)

### Symptom
`make download-philosophy-wiki` viselo na `prošlo_stránek=4`, `nalezeno=0`. Přitom SQLite deadlock nebyl příčina — counter `scanned` se inkrementoval před zápisem do DB.

### Příčiny (v pořadí odkrytí)

1. **HTML entity v CS Wikipedia dumpu**
   Go `encoding/xml` zná jen 5 standardních XML entit (`&amp; &lt; &gt; &apos; &quot;`). CS Wikipedia text obsahuje desítky HTML entit (`&nbsp;` `&mdash;` `&hellip;` atd.). S `Strict=false` je parser tiše zahazoval → stránky se zdánlivě parsovaly, ale text byl prázdný nebo ořezaný.

2. **Pokus o line-based scanner (slepá ulička)**
   Plán 01 zkusil nahradit `encoding/xml` ručním `bytes.Index("<page>")` scannerem. Ten byl podezřele rychlý, ale v `else` větvi pro víceřádkové `<page>` bloky byl přidán dočasný bypass (`_ = pageBuf.Bytes(); _ = fn`) — callback se **nikdy nevolal** pro víceřádkové stránky (tj. pro 99,9 % obsahu).

3. **Inline topic filter bránil iteraci**
   Každá změna klíčových slov = reparsování 1.27 GB bzip2 dumpu. Nelze debugovat co dump obsahuje bez SQL.

### Řešení

#### Oprava parseru (`internal/wiki/downloader.go`)
Přepis Fáze 2 podle funkčního prototypu v `~/Dev/WikiImport/wiki_import.go`:
- `lbzip2` s fallbackem na `bzip2` (detekce přes `exec.LookPath`)
- `encoding/xml` decoder s `Strict=false`
- **`sanitizeReader`** — obaluje reader a PŘED XML parserem nahrazuje ~90 HTML entit unicode ekvivalenty a zahazuje neplatné XML 1.0 control chars. Bez toho celá oprava nefunguje.
- Carry-over `tail` buffer (16 bajtů) chrání před rozseknutím entity na hranici read() volání.
- Bytový progress counter (`progressReader`) s diagnostickým tickerem každých 10s: `rozbaleno_MB / prošlo_stránek / nalezeno`

#### Dvoufázová architektura (nový vzor)

**Stará architektura:**
```
dump → parser → matchesTopic() filter → cleanWikitext() → chunk → documents (rag_edu.db)
```
Problém: filter inline = re-parse pro každou iteraci, nelze debugovat.

**Nová architektura:**
```
Fáze 1 (jednou):  dump → parser → wiki_<lang>.db  (raw articles, 550k CS článků)
Fáze 2 (iterace): wiki_<lang>.db → SQL WHERE → CleanWikitext → ChunkText → rag_edu.db
```

## Implementované změny

### `internal/wiki/downloader.go`
- `Page` → `Article` (přidán `ID int64`, `RawText string`)
- `xmlPage` dostala `ID int64 \`xml:"id"\``
- Odstraněno: `DefaultTopics`, `DownloadConfig.Topics`, `matchesTopic()`
- `Download()` callback: `func(*Article)` — předává raw wikitext bez filtru
- `cleanWikitext` → exportovaná `CleanWikitext` (volá ji wiki-filter)
- Přidán `sanitizeReader` + `htmlEntities` tabulka (~90 entit) + `sanitize()`
- `sanitizeReader.Read()` implementuje carry-over `tail` buffer pro bezpečné hranice

### `internal/storage/wiki_db.go` (NOVÝ)
- `WikiDB` + `Article` struct
- `OpenWikiDB()` — DELETE journal mode (jako `db.go`, kvůli macOS shm mutex)
- `SaveArticleBatch()` — INSERT OR IGNORE, transakce, timeout 60s
- `QueryArticles(whereSQL, args, limit)` — parametrizovaný SQL query
- `CountArticles()`

### `cmd/downloader/main.go`
- `runWiki()` přepsáno: ukládá do `wiki_<lang>.db` (raw, bez chunking)
- Batch: 10 000 článků na transakci
- Odstraněn `--topics` flag pro wiki mode

### `cmd/wiki-filter/main.go` (NOVÝ)
- `--wiki-db`, `--rag-db`, `--lang`
- `--topics "A,B,C"` — generuje `(title LIKE ? OR raw_text LIKE ?)` per keyword
- `--where` — raw SQL WHERE (pro pokročilé dotazy)
- `--limit`, `--chunk-size` (default 1500), `--chunk-overlap` (default 150), `--min-chunk-len` (default 100)
- Flow: `QueryArticles` → `wiki.CleanWikitext` → `pipeline.ChunkText` → `storage.SaveDocumentBatch`
- Batch: 1 000 dokumentů na commit

### Makefile
- `download-wiki`, `download-wiki-cs`: odebrán `--max-pages` (ukládá vše)
- Nový `download-wiki-en` target (s varováním o ~20GB dumpu)
- `filter-philosophy` (nový): volá `cmd/wiki-filter` s CS filozofickými tématy
- `download-philosophy-wiki`: teď = `download-wiki-cs` + `filter-philosophy`

## Verifikace (ověřeno)

```bash
# Parser — 1000 článků za 1s, lbzip2 funguje
go run cmd/downloader/main.go --source wiki --lang cs --max-pages 1000 --data-dir ./data --log info
# → rozbaleno_MB=21.1 prošlo_stránek=1306 nalezeno=1000 elapsed=1s

# SQL filter — vidíme co dump obsahuje
sqlite3 data/wiki_cs.db "SELECT id, title FROM articles WHERE title LIKE '%stoic%' LIMIT 10;"

# Filter → rag_edu.db
go run cmd/wiki-filter/main.go --wiki-db ./data/wiki_cs.db --rag-db ./data/rag_edu.db \
  --lang cs --topics "Stoicismus,Seneca,Filozofie"
```

## Zbývající pre-existing bug (nesouvisí)

`internal/pipeline/processor_test.go:TestChunkText_MultipleChunks` způsobuje nekonečnou smyčku v `ChunkText()` při specifickém vstupu (processor.go:116). Netýká se tohoto plánu.
