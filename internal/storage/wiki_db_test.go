// internal/storage/wiki_db_test.go
package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tempWikiDB(t *testing.T) (*WikiDB, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "wiki_db_test_*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	w, err := OpenWikiDB(filepath.Join(dir, "wiki.db"))
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("OpenWikiDB: %v", err)
	}
	return w, func() {
		w.Close()
		os.RemoveAll(dir)
	}
}

func TestWikiDB_SaveAndCount(t *testing.T) {
	w, cleanup := tempWikiDB(t)
	defer cleanup()

	arts := []*Article{
		{ID: 1, Title: "Stoicismus", RawText: "Filozofický směr antického Říma."},
		{ID: 2, Title: "Seneca", RawText: "Římský filozof a politik."},
		{ID: 3, Title: "Fotosyntéza", RawText: "Proces přeměny sluneční energie."},
	}

	n, err := w.SaveArticleBatch(arts)
	if err != nil {
		t.Fatalf("SaveArticleBatch: %v", err)
	}
	if n != 3 {
		t.Errorf("saved = %d, want 3", n)
	}

	count, err := w.CountArticles()
	if err != nil {
		t.Fatalf("CountArticles: %v", err)
	}
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
}

func TestWikiDB_DedupByID(t *testing.T) {
	w, cleanup := tempWikiDB(t)
	defer cleanup()

	// První batch
	if _, err := w.SaveArticleBatch([]*Article{
		{ID: 42, Title: "Stoicismus", RawText: "Version 1"},
	}); err != nil {
		t.Fatalf("first save: %v", err)
	}

	// Druhý batch — stejné ID, jiný text; INSERT OR IGNORE → druhý zápis nebude uložen
	n, err := w.SaveArticleBatch([]*Article{
		{ID: 42, Title: "Stoicismus updated", RawText: "Version 2"},
	})
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if n != 0 {
		t.Errorf("duplicate save should return 0 rows affected, got %d", n)
	}

	// Ověř že text je stále původní
	arts, err := w.QueryArticles("id = ?", []any{42}, 0)
	if err != nil {
		t.Fatalf("QueryArticles: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("expected 1 article, got %d", len(arts))
	}
	if arts[0].RawText != "Version 1" {
		t.Errorf("text should be 'Version 1', got %q", arts[0].RawText)
	}
}

func TestWikiDB_QueryWithWhere(t *testing.T) {
	w, cleanup := tempWikiDB(t)
	defer cleanup()

	_, err := w.SaveArticleBatch([]*Article{
		{ID: 1, Title: "Stoicismus", RawText: "filosofie antiky"},
		{ID: 2, Title: "Fotosyntéza", RawText: "biologie rostlin"},
		{ID: 3, Title: "Seneca", RawText: "římský stoik"},
		{ID: 4, Title: "Pythagoras", RawText: "řecký matematik"},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	// LIKE filter na raw_text
	arts, err := w.QueryArticles("raw_text LIKE ?", []any{"%stoik%"}, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(arts) != 1 || arts[0].Title != "Seneca" {
		t.Errorf("expected 1 match 'Seneca', got %v", arts)
	}

	// Více OR podmínek
	arts, err = w.QueryArticles("title LIKE ? OR title LIKE ?", []any{"%toic%", "%enec%"}, 0)
	if err != nil {
		t.Fatalf("query OR: %v", err)
	}
	if len(arts) != 2 {
		t.Errorf("expected 2 matches, got %d", len(arts))
	}

	// Limit
	arts, err = w.QueryArticles("", nil, 2)
	if err != nil {
		t.Fatalf("query limit: %v", err)
	}
	if len(arts) != 2 {
		t.Errorf("expected limit=2, got %d", len(arts))
	}
}

func TestWikiDB_AutoImportedAt(t *testing.T) {
	w, cleanup := tempWikiDB(t)
	defer cleanup()

	before := time.Now().Add(-time.Second)
	_, err := w.SaveArticleBatch([]*Article{
		{ID: 1, Title: "Test", RawText: "content"},
		// ImportedAt záměrně nevyplněno
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	after := time.Now().Add(time.Second)

	arts, err := w.QueryArticles("", nil, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("expected 1 article")
	}
	if arts[0].ImportedAt.Before(before) || arts[0].ImportedAt.After(after) {
		t.Errorf("ImportedAt out of expected range: %v (before %v, after %v)",
			arts[0].ImportedAt, before, after)
	}
}
