// internal/storage/wiki_db.go
// Samostatný SQLite store pro raw Wikipedia články (jeden soubor per jazyk).
// Cílem je parse-all vzor: dump → wiki_<lang>.db (jednou, drahé),
// pak SQL filter + clean + chunk → rag_edu.db documents (levně, často).
package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Article raw Wikipedia článek před cleaningem/chunkingem.
type Article struct {
	ID         int64     // Wikipedia page ID — kanonický klíč (unikátní per dump)
	Title      string
	NS         int       // namespace (v praxi vždy 0 — downloader ne-článkové filtruje)
	RawText    string    // nezpracovaný wikitext
	ImportedAt time.Time
}

// WikiDB storage pro raw wiki články.
type WikiDB struct {
	db *sql.DB
}

// OpenWikiDB otevře nebo vytvoří wiki SQLite databázi.
// Stejné PRAGMAs jako DB.Open (DELETE journal kvůli macOS shm mutex bugu,
// single connection, temp_store=MEMORY).
func OpenWikiDB(path string) (*WikiDB, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=DELETE&_busy_timeout=10000")
	if err != nil {
		return nil, fmt.Errorf("open wiki db: %w", err)
	}
	db.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode=DELETE",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=10000",
		"PRAGMA cache_size=-131072", // 128 MB page cache (bulk import)
		"PRAGMA temp_store=MEMORY",
		"PRAGMA mmap_size=1073741824", // 1 GB mmap
	} {
		if _, err := db.Exec(pragma); err != nil {
			return nil, fmt.Errorf("pragma %q: %w", pragma, err)
		}
	}

	w := &WikiDB{db: db}
	if err := w.migrate(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *WikiDB) migrate() error {
	_, err := w.db.Exec(`
		CREATE TABLE IF NOT EXISTS articles (
			id          INTEGER PRIMARY KEY,
			title       TEXT NOT NULL,
			ns          INTEGER NOT NULL DEFAULT 0,
			raw_text    TEXT NOT NULL,
			imported_at DATETIME NOT NULL
		);

		CREATE INDEX IF NOT EXISTS idx_articles_title ON articles(title);
	`)
	return err
}

// SaveArticleBatch uloží batch článků v jedné transakci.
// Dedup přes PRIMARY KEY(id) + INSERT OR IGNORE — duplikáty jsou tiché.
// Vrací počet skutečně vložených řádků.
func (w *WikiDB) SaveArticleBatch(arts []*Article) (int, error) {
	if len(arts) == 0 {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR IGNORE INTO articles (id, title, ns, raw_text, imported_at)
		VALUES (?, ?, ?, ?, ?)
	`)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	defer stmt.Close()

	saved := 0
	for _, a := range arts {
		if a.ImportedAt.IsZero() {
			a.ImportedAt = time.Now()
		}
		res, err := stmt.ExecContext(ctx, a.ID, a.Title, a.NS, a.RawText, a.ImportedAt)
		if err != nil {
			tx.Rollback()
			return saved, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			saved++
		}
	}
	return saved, tx.Commit()
}

// CountArticles vrátí celkový počet článků v DB.
func (w *WikiDB) CountArticles() (int64, error) {
	var n int64
	err := w.db.QueryRow("SELECT COUNT(*) FROM articles").Scan(&n)
	return n, err
}

// QueryArticles vrátí články odpovídající volitelné SQL WHERE klauzuli.
// whereSQL je jen obsah za klíčovým slovem WHERE (bez něj). Pokud je prázdný,
// vrací vše. limit 0 = bez limitu.
//
// Bezpečnost: caller je zodpovědný za to, aby whereSQL neobsahoval uživatelský vstup.
// V praxi whereSQL sestavuje cmd/wiki-filter z flagů (--topics generují LIKE
// výrazy s parametrizovanými args).
func (w *WikiDB) QueryArticles(whereSQL string, args []any, limit int) ([]*Article, error) {
	query := "SELECT id, title, ns, raw_text, imported_at FROM articles"
	if whereSQL != "" {
		query += " WHERE " + whereSQL
	}
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := w.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Article
	for rows.Next() {
		var a Article
		var importedStr string
		if err := rows.Scan(&a.ID, &a.Title, &a.NS, &a.RawText, &importedStr); err != nil {
			return nil, err
		}
		a.ImportedAt, _ = time.Parse(time.RFC3339Nano, importedStr)
		if a.ImportedAt.IsZero() {
			a.ImportedAt, _ = time.Parse("2006-01-02 15:04:05.999999999-07:00", importedStr)
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

// Close zavře databázi.
func (w *WikiDB) Close() error {
	return w.db.Close()
}
