// internal/storage/db_test.go
package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tempDB(t *testing.T) (*DB, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "rag_edu_test_*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}

	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("Open: %v", err)
	}

	return db, func() {
		db.Close()
		os.RemoveAll(dir)
	}
}

func TestSaveDocument_Basic(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	doc := &Document{
		ID:        "test_001",
		Source:    "wikipedia",
		Lang:      "en",
		Group:     "science",
		Title:     "Photosynthesis",
		Text:      "Photosynthesis is the process by which plants convert sunlight into energy.",
		CreatedAt: time.Now(),
	}

	if err := db.SaveDocument(doc); err != nil {
		t.Fatalf("SaveDocument: %v", err)
	}

	count, err := db.CountDocuments("wikipedia")
	if err != nil {
		t.Fatalf("CountDocuments: %v", err)
	}
	if count != 1 {
		t.Errorf("Očekáván count=1, dostal %d", count)
	}
}

func TestSaveDocument_Deduplication(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	doc := &Document{
		Source: "usenet",
		Lang:   "en",
		Group:  "sci.bio",
		Title:  "Re: plants",
		Text:   "Plants need water and sunlight to grow properly in your garden.",
	}

	// První uložení
	if err := db.SaveDocument(doc); err != nil {
		t.Fatalf("První SaveDocument: %v", err)
	}

	// Druhé uložení stejného textu — mělo by být ignorováno (dedup)
	doc2 := *doc
	doc2.ID = "different_id"
	if err := db.SaveDocument(&doc2); err != nil {
		t.Fatalf("Druhý SaveDocument: %v", err)
	}

	count, _ := db.CountDocuments("")
	if count != 1 {
		t.Errorf("Duplicitní dokument by neměl být uložen, count=%d", count)
	}
}

func TestSaveDocument_AutoID(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	doc := &Document{
		// ID záměrně nevyplněno — mělo být vygenerováno
		Source: "wikipedia",
		Lang:   "en",
		Group:  "history",
		Title:  "Ancient Egypt",
		Text:   "Ancient Egypt was one of the most powerful civilizations in the ancient world.",
	}

	if err := db.SaveDocument(doc); err != nil {
		t.Fatalf("SaveDocument: %v", err)
	}

	count, _ := db.CountDocuments("")
	if count != 1 {
		t.Errorf("Očekáván count=1, dostal %d", count)
	}
}

func TestExportJSONL(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	// Vlož několik dokumentů
	for i := 0; i < 5; i++ {
		doc := &Document{
			Source: "wikipedia",
			Lang:   "en",
			Group:  "science",
			Title:  "Test article",
			Text:   "This is test content number " + string(rune('0'+i)) + " with enough text to pass the filter.",
		}
		if err := db.SaveDocument(doc); err != nil {
			t.Fatalf("SaveDocument %d: %v", i, err)
		}
	}

	// Export do temp souboru
	dir, _ := os.MkdirTemp("", "export_test_*")
	defer os.RemoveAll(dir)
	outPath := filepath.Join(dir, "docs.jsonl")

	count, err := db.ExportJSONL(outPath, 100)
	if err != nil {
		t.Fatalf("ExportJSONL: %v", err)
	}

	if count != 5 {
		t.Errorf("Očekáván export 5 dokumentů, dostal %d", count)
	}

	// Zkontroluj soubor
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) == 0 {
		t.Error("JSONL soubor je prázdný")
	}
}

func TestSetGetState(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	// Nastavit state
	if err := db.SetState("nntp_last_sci.bio", "12345"); err != nil {
		t.Fatalf("SetState: %v", err)
	}

	// Načíst
	val, ok := db.GetState("nntp_last_sci.bio")
	if !ok {
		t.Fatal("GetState: klíč nenalezen")
	}
	if val != "12345" {
		t.Errorf("Očekávána hodnota '12345', dostal '%s'", val)
	}

	// Přepsat
	if err := db.SetState("nntp_last_sci.bio", "99999"); err != nil {
		t.Fatalf("SetState update: %v", err)
	}
	val, _ = db.GetState("nntp_last_sci.bio")
	if val != "99999" {
		t.Errorf("Očekávána přepsaná hodnota '99999', dostal '%s'", val)
	}

	// Neexistující klíč
	_, ok = db.GetState("nonexistent_key")
	if ok {
		t.Error("Neexistující klíč by neměl být nalezen")
	}
}

func TestDocumentHash_Consistency(t *testing.T) {
	doc1 := &Document{Text: "Hello world, this is a test."}
	doc2 := &Document{Text: "Hello world, this is a test."}
	doc3 := &Document{Text: "Different text entirely."}

	if doc1.Hash() != doc2.Hash() {
		t.Error("Stejný text by měl mít stejný hash")
	}
	if doc1.Hash() == doc3.Hash() {
		t.Error("Různé texty by měly mít různé hashe")
	}
}

func TestCountDocuments_FilterBySource(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	sources := []string{"wikipedia", "wikipedia", "usenet", "usenet", "usenet"}
	for i, src := range sources {
		doc := &Document{
			Source: src,
			Lang:   "en",
			Group:  "test",
			Title:  "Article",
			Text:   "Content " + string(rune('a'+i)) + " test text for the database storage layer unit test.",
		}
		db.SaveDocument(doc)
	}

	wikiCount, _ := db.CountDocuments("wikipedia")
	if wikiCount != 2 {
		t.Errorf("Očekávány 2 Wikipedia dokumenty, dostal %d", wikiCount)
	}

	nntpCount, _ := db.CountDocuments("usenet")
	if nntpCount != 3 {
		t.Errorf("Očekávány 3 Usenet dokumenty, dostal %d", nntpCount)
	}

	totalCount, _ := db.CountDocuments("")
	if totalCount != 5 {
		t.Errorf("Očekáváno 5 celkem, dostal %d", totalCount)
	}
}
