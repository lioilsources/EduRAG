// cmd/processor/main.go
// Exportuje zpracovaná data z SQLite do JSONL pro Python embedding pipeline.
// Spouštěj po downloadu, před python embedder/embed.py
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/yourname/rag-edu/internal/storage"
)

func main() {
	dataDir   := flag.String("data-dir", "./data", "Složka s daty")
	batchSize := flag.Int("batch", 10000, "Max dokumentů na export")
	flag.Parse()

	db, err := storage.Open(*dataDir + "/rag_edu.db")
	if err != nil {
		slog.Error("open db", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	total, _ := db.CountDocuments("")
	pending, _ := db.CountDocuments("") // TODO: count by embedded=0

	slog.Info("DB stav", "total_documents", total, "pending_embed", pending)

	outputPath := *dataDir + "/processed/documents.jsonl"
	if err := os.MkdirAll(*dataDir+"/processed", 0755); err != nil {
		slog.Error("mkdir", "err", err)
		os.Exit(1)
	}

	count, err := db.ExportJSONL(outputPath, *batchSize)
	if err != nil {
		slog.Error("export failed", "err", err)
		os.Exit(1)
	}

	slog.Info("Export hotov",
		"output", outputPath,
		"documents", count,
	)

	fmt.Printf("\nDalší krok:\n")
	fmt.Printf("  python python/embedder/embed.py --input %s --db %s/chromadb\n",
		outputPath, *dataDir)
}
