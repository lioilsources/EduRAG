// cmd/wiki-filter/main.go
// Druhá fáze wiki pipeline: čte raw články z wiki_<lang>.db, filtruje SQL,
// čistí wikitext, chunkuje a zapisuje do rag_edu.db documents table.
//
// Návazná fáze po cmd/downloader. Umožňuje iterovat na topic filtru bez
// znovu-parsování dumpu.
//
// Příklady:
//   # filtr přes --topics (vygeneruje LIKE '%X%' OR … pro title i raw_text)
//   go run cmd/wiki-filter/main.go \
//       --wiki-db ./data/wiki_cs.db --rag-db ./data/rag_edu.db --lang cs \
//       --topics "stoicismus,seneca,filozofie"
//
//   # raw SQL WHERE
//   go run cmd/wiki-filter/main.go \
//       --wiki-db ./data/wiki_cs.db --rag-db ./data/rag_edu.db --lang cs \
//       --where "title LIKE '%stoic%' OR raw_text LIKE '%stoicismus%'"
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/yourname/rag-edu/internal/pipeline"
	"github.com/yourname/rag-edu/internal/storage"
	"github.com/yourname/rag-edu/internal/wiki"
)

func main() {
	wikiDBPath := flag.String("wiki-db", "./data/wiki_cs.db", "Zdrojová wiki SQLite DB")
	ragDBPath := flag.String("rag-db", "./data/rag_edu.db", "Cílová RAG SQLite DB (documents tabulka)")
	lang := flag.String("lang", "cs", "Jazyk (cs/en/simple) — zapíše se do documents.lang")
	topics := flag.String("topics", "", "Čárkou oddělená klíčová slova (generuje LIKE přes title+raw_text)")
	where := flag.String("where", "", "Raw SQL WHERE klauzule (alternativa k --topics)")
	limit := flag.Int("limit", 0, "Max článků (0 = vše)")
	chunkSize := flag.Int("chunk-size", 1500, "Velikost chunku v znacích")
	chunkOverlap := flag.Int("chunk-overlap", 150, "Překryv chunků v znacích")
	minChunkLen := flag.Int("min-chunk-len", 100, "Minimální délka chunku — kratší se zahodí")
	group := flag.String("group", "encyclopedia", "Hodnota pro documents.group")
	logLevel := flag.String("log", "info", "Log level: debug|info|warn")
	flag.Parse()

	var level slog.Level
	switch *logLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	default:
		level = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	// --- Postav WHERE klauzuli ---
	var whereSQL string
	var whereArgs []any

	switch {
	case *where != "" && *topics != "":
		slog.Error("použij buď --where nebo --topics, ne obojí")
		os.Exit(1)
	case *where != "":
		whereSQL = *where
	case *topics != "":
		whereSQL, whereArgs = buildTopicsWhere(*topics)
	}

	slog.Info("Wiki filter start",
		"wiki_db", *wikiDBPath,
		"rag_db", *ragDBPath,
		"lang", *lang,
		"where", whereSQL,
		"limit", *limit,
	)

	// --- Otevři oba DB ---
	wikiDB, err := storage.OpenWikiDB(*wikiDBPath)
	if err != nil {
		slog.Error("open wiki db", "path", *wikiDBPath, "err", err)
		os.Exit(1)
	}
	defer wikiDB.Close()

	total, _ := wikiDB.CountArticles()
	slog.Info("Zdrojová DB", "total_articles", total)

	ragDB, err := storage.Open(*ragDBPath)
	if err != nil {
		slog.Error("open rag db", "path", *ragDBPath, "err", err)
		os.Exit(1)
	}
	defer ragDB.Close()

	// --- Query + filter + clean + chunk + save ---
	start := time.Now()
	articles, err := wikiDB.QueryArticles(whereSQL, whereArgs, *limit)
	if err != nil {
		slog.Error("query articles", "err", err)
		os.Exit(1)
	}
	slog.Info("Match", "articles", len(articles), "elapsed", time.Since(start).Round(time.Millisecond))

	const docBatchSize = 1000
	docBatch := make([]*storage.Document, 0, docBatchSize)
	totalChunks := 0
	saved := 0
	tooShort := 0

	flush := func() {
		if len(docBatch) == 0 {
			return
		}
		n, err := ragDB.SaveDocumentBatch(docBatch)
		if err != nil {
			slog.Error("save document batch", "err", err, "batch_size", len(docBatch))
		}
		saved += n
		docBatch = docBatch[:0]
	}

	for i, a := range articles {
		cleaned := wiki.CleanWikitext(a.RawText)
		if len(cleaned) < *minChunkLen {
			tooShort++
			continue
		}
		chunks := pipeline.ChunkText(cleaned, *chunkSize, *chunkOverlap)
		for j, chunk := range chunks {
			if len([]rune(chunk)) < *minChunkLen {
				continue
			}
			title := a.Title
			if len(chunks) > 1 {
				title = fmt.Sprintf("%s (část %d)", a.Title, j+1)
			}
			docBatch = append(docBatch, &storage.Document{
				Source: "wikipedia",
				Lang:   *lang,
				Group:  *group,
				Title:  title,
				Text:   chunk,
			})
			totalChunks++
			if len(docBatch) >= docBatchSize {
				flush()
			}
		}
		if (i+1)%1000 == 0 {
			slog.Info("Progress",
				"processed", i+1,
				"of", len(articles),
				"chunks_saved", saved,
				"elapsed", time.Since(start).Round(time.Second),
			)
		}
	}
	flush()

	slog.Info("Wiki filter hotov",
		"articles_in", len(articles),
		"too_short", tooShort,
		"chunks_generated", totalChunks,
		"chunks_saved", saved,
		"elapsed", time.Since(start).Round(time.Second),
	)
}

// buildTopicsWhere sestaví SQL fragment ve tvaru:
//
//	(title LIKE ? OR raw_text LIKE ?) OR (title LIKE ? OR raw_text LIKE ?) OR …
//
// s parametrizovanými %topic% argumenty. Každé topic je lowercase — ale LIKE je
// v SQLite defaultně case-insensitive jen pro ASCII, takže pro české znaky
// (á, ř, š) match NEBUDE case-insensitive. Uživatel by měl zadávat topic v té
// podobě, v jaké se vyskytuje v dumpu (Wikipedia titulky jsou typicky
// kapitalizované: "Stoicismus", "Seneca").
func buildTopicsWhere(topics string) (string, []any) {
	parts := strings.Split(topics, ",")
	var clauses []string
	var args []any
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		clauses = append(clauses, "(title LIKE ? OR raw_text LIKE ?)")
		like := "%" + p + "%"
		args = append(args, like, like)
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return strings.Join(clauses, " OR "), args
}
