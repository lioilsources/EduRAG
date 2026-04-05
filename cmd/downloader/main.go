// cmd/downloader/main.go
// CLI nástroj pro stahování dat z Usenet a Wikipedia.
//
// Použití:
//   go run cmd/downloader/main.go --source wiki --lang simple
//   go run cmd/downloader/main.go --source nntp --server news.example.com --groups "sci.bio,rec.gardens"
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/yourname/rag-edu/internal/nntp"
	"github.com/yourname/rag-edu/internal/pipeline"
	"github.com/yourname/rag-edu/internal/storage"
	"github.com/yourname/rag-edu/internal/wiki"
)

func main() {
	// --- Flagy ---
	source     := flag.String("source", "wiki", "Zdroj dat: 'wiki' nebo 'nntp'")
	dataDir    := flag.String("data-dir", "./data", "Složka pro data")
	lang       := flag.String("lang", "simple", "Jazyk Wikipedia: 'simple', 'cs', 'en'")
	maxPages   := flag.Int("max-pages", 50000, "Max počet Wikipedia stránek (0 = vše)")
	nntpServer := flag.String("server", "", "NNTP server hostname")
	nntpPort   := flag.Int("port", 119, "NNTP port (563=TLS, 119=plain)")
	nntpTLS    := flag.Bool("tls", true, "Použít TLS")
	nntpUser   := flag.String("user", "", "NNTP username")
	nntpPass   := flag.String("pass", "", "NNTP password")
	groups     := flag.String("groups", "sci.bio,sci.geo.meteorology,rec.gardens,humanities.philosophy,soc.history,alt.history", "Usenet skupiny (čárkou oddělené)")
	maxArticles := flag.Int("max-articles", 5000, "Max článků na skupinu")
	topics     := flag.String("topics", "", "Filtrovací klíčová slova pro Wikipedia (čárkou oddělená; prázdné = výchozí)")
	workers    := flag.Int("workers", 4, "Počet paralelních workerů pro NNTP")
	logLevel   := flag.String("log", "info", "Log level: debug|info|warn")
	flag.Parse()

	// --- Logger ---
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

	// --- Storage ---
	if err := os.MkdirAll(*dataDir, 0755); err != nil {
		slog.Error("create data dir", "err", err)
		os.Exit(1)
	}
	db, err := storage.Open(*dataDir + "/rag_edu.db")
	if err != nil {
		slog.Error("open db", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	proc := pipeline.NewProcessor(pipeline.DefaultConfig())

	var topicList []string
	if *topics != "" {
		for _, t := range strings.Split(*topics, ",") {
			if t = strings.TrimSpace(t); t != "" {
				topicList = append(topicList, strings.ToLower(t))
			}
		}
	}

	switch *source {
	case "wiki":
		runWiki(db, proc, *dataDir, *lang, *maxPages, topicList)
	case "nntp":
		if *nntpServer == "" {
			slog.Error("--server je povinný pro NNTP")
			os.Exit(1)
		}
		groupList := strings.Split(*groups, ",")
		runNNTP(db, proc, *nntpServer, *nntpPort, *nntpTLS, *nntpUser, *nntpPass, groupList, *maxArticles, *workers)
	default:
		slog.Error("Neznámý source", "source", *source)
		os.Exit(1)
	}

	count, _ := db.CountDocuments("")
	slog.Info("Hotovo", "total_documents", count)
}

func runWiki(db *storage.DB, proc *pipeline.Processor, dataDir, lang string, maxPages int, topics []string) {
	urls := map[string]string{
		"simple": wiki.SimpleEnDumpURL,
		"cs":     wiki.CsDumpURL,
		"en":     wiki.EnDumpURL,
	}

	url, ok := urls[lang]
	if !ok {
		slog.Error("Neznámý lang pro wiki", "lang", lang)
		os.Exit(1)
	}

	cfg := wiki.DownloadConfig{
		URL:       url,
		OutputDir: dataDir + "/raw",
		MaxPages:  maxPages,
		Topics:    topics,
		Lang:      lang,
	}

	downloader := wiki.NewDownloader(cfg)

	saved := 0
	skipped := 0

	err := downloader.Download(func(page *wiki.Page) error {
		text := proc.ProcessWiki(page.Text)
		if text == "" {
			skipped++
			return nil
		}

		chunks := pipeline.ChunkText(text, 1500, 150)

		var batch []*storage.Document
		for i, chunk := range chunks {
			title := page.Title
			if len(chunks) > 1 {
				title = fmt.Sprintf("%s (část %d)", page.Title, i+1)
			}
			batch = append(batch, &storage.Document{
				Source: "wikipedia",
				Lang:   lang,
				Group:  "encyclopedia",
				Title:  title,
				Text:   chunk,
			})
		}

		n, err := db.SaveDocumentBatch(batch)
		if err != nil {
			slog.Debug("batch save error", "title", page.Title, "err", err)
		}
		saved += n
		return nil
	})

	if err != nil {
		slog.Error("Wiki chyba", "err", err)
	}
	slog.Info("Wiki import hotov", "saved", saved, "skipped", skipped)
}

func runNNTP(
	db *storage.DB, proc *pipeline.Processor,
	server string, port int, useTLS bool,
	user, pass string,
	groups []string, maxArticles, workers int,
) {
	cfg := nntp.Config{
		Server:   server,
		Port:     port,
		UseTLS:   useTLS,
		Username: user,
		Password: pass,
		Timeout:  30 * time.Second,
	}

	// Test spojení
	testClient := nntp.NewClient(cfg)
	if err := testClient.Connect(); err != nil {
		slog.Error("NNTP connect failed", "err", err)
		os.Exit(1)
	}

	for _, group := range groups {
		group = strings.TrimSpace(group)
		slog.Info("Zpracovávám skupinu", "group", group)

		// Zjisti rozsah článků
		info, err := testClient.GetGroup(group)
		if err != nil {
			slog.Error("GetGroup failed", "group", group, "err", err)
			continue
		}

		slog.Info("Skupina info",
			"group", group,
			"count", info.Count,
			"first", info.First,
			"last", info.Last,
		)

		// Omez na posledních N článků
		first := info.Last - int64(maxArticles) + 1
		if first < info.First {
			first = info.First
		}

		// Zkontroluj resume state
		stateKey := "nntp_last_" + group
		if lastStr, ok := db.GetState(stateKey); ok {
			var last int64
			if _, err := parseIntFromString(lastStr, &last); err == nil && last > first {
				first = last + 1
				slog.Info("Resuming from", "group", group, "first", first)
			}
		}

		if first > info.Last {
			slog.Info("Skupina je aktuální, přeskakuji", "group", group)
			continue
		}

		articles, errs := testClient.FetchRange(group, first, info.Last, workers)

		saved := 0
		skipped := 0
		processed := 0
		total := int(info.Last - first + 1)
		groupStart := time.Now()

		for article := range articles {
			processed++
			if processed%100 == 0 {
				slog.Info("NNTP progress",
					"group", group,
					"zpracováno", processed,
					"celkem", total,
					"uloženo", saved,
					"elapsed", time.Since(groupStart).Round(time.Second),
				)
			}
			text := proc.ProcessUsenet(article.Body)
			if text == "" {
				skipped++
				continue
			}

			if !pipeline.IsEducational(text) {
				skipped++
				continue
			}

			lang := pipeline.DetectLanguage(text)

			doc := &storage.Document{
				Source: "usenet",
				Lang:   lang,
				Group:  article.Newsgroup,
				Title:  article.Subject,
				Text:   text,
			}
			doc.ID = "nntp_" + article.MessageID

			if err := db.SaveDocument(doc); err != nil {
				skipped++
				continue
			}
			saved++
		}

		if err := <-errs; err != nil {
			slog.Error("NNTP error", "group", group, "err", err)
		}

		// Ulož stav pro resume
		db.SetState(stateKey, formatInt(info.Last))

		slog.Info("Skupina hotova",
			"group", group,
			"saved", saved,
			"skipped", skipped,
		)
	}

	testClient.Close()
}

func parseIntFromString(s string, out *int64) (int, error) {
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	if err == nil {
		*out = n
	}
	return 0, err
}

func formatInt(n int64) string {
	return fmt.Sprintf("%d", n)
}
