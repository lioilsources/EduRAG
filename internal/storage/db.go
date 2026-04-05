// internal/storage/db.go
// SQLite storage pro stažené dokumenty.
// Zajišťuje deduplikaci, stav zpracování a export do JSONL.
package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Document jeden zpracovaný dokument připravený pro embedding.
type Document struct {
	ID        string    `json:"id"`
	Source    string    `json:"source"`  // "usenet" | "wikipedia"
	Lang      string    `json:"lang"`    // "en" | "cs" | "simple"
	Group     string    `json:"group"`   // newsgroup nebo wiki kategorie
	Title     string    `json:"title"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
	Embedded  bool      `json:"embedded"`
}

// Hash vrátí SHA256 hash textu pro deduplikaci.
func (d *Document) Hash() string {
	h := sha256.Sum256([]byte(d.Text))
	return fmt.Sprintf("%x", h[:8]) // Prvních 8 bytů stačí
}

// DB SQLite storage.
type DB struct {
	db *sql.DB
}

// Open otevře nebo vytvoří SQLite databázi.
func Open(path string) (*DB, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=10000")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// SQLite nepodporuje souběžné zápisy — force single connection
	db.SetMaxOpenConns(1)

	// DELETE mode: jednoduché POSIX file locks, žádný shm soubor,
	// uvolní se automaticky při death procesu (i SIGKILL).
	// WAL mode má problémy se shm mutexy na macOS při concurrent procesech.
	for _, pragma := range []string{
		"PRAGMA journal_mode=DELETE",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=10000",
		"PRAGMA cache_size=-64000",
		"PRAGMA temp_store=MEMORY",
	} {
		if _, err := db.Exec(pragma); err != nil {
			return nil, fmt.Errorf("pragma %q: %w", pragma, err)
		}
	}

	store := &DB{db: db}
	if err := store.migrate(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *DB) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS documents (
			id          TEXT PRIMARY KEY,
			source      TEXT NOT NULL,
			lang        TEXT NOT NULL,
			grp         TEXT NOT NULL DEFAULT '',
			title       TEXT NOT NULL,
			text        TEXT NOT NULL,
			text_hash   TEXT NOT NULL,
			created_at  DATETIME NOT NULL,
			embedded    INTEGER NOT NULL DEFAULT 0
		);

		CREATE INDEX IF NOT EXISTS idx_source ON documents(source);
		CREATE INDEX IF NOT EXISTS idx_embedded ON documents(embedded);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_hash ON documents(text_hash);

		CREATE TABLE IF NOT EXISTS download_state (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
	`)
	return err
}

// SaveDocument uloží dokument, ignoruje duplikáty (podle hash).
func (s *DB) SaveDocument(d *Document) error {
	if d.ID == "" {
		d.ID = fmt.Sprintf("%s_%s", d.Source, d.Hash())
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now()
	}

	_, err := s.db.Exec(`
		INSERT OR IGNORE INTO documents
			(id, source, lang, grp, title, text, text_hash, created_at, embedded)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)
	`, d.ID, d.Source, d.Lang, d.Group, d.Title, d.Text, d.Hash(), d.CreatedAt)

	return err
}

// SaveDocumentBatch uloží více dokumentů v jedné transakci — mnohem rychlejší než volat SaveDocument opakovaně.
func (s *DB) SaveDocumentBatch(docs []*Document) (int, error) {
	if len(docs) == 0 {
		return 0, nil
	}
	// Context timeout — nikdy neblokuje déle než 30s (chrání před SQLite lock deadlockem)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR IGNORE INTO documents
			(id, source, lang, grp, title, text, text_hash, created_at, embedded)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)
	`)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	defer stmt.Close()

	saved := 0
	for _, d := range docs {
		if d.ID == "" {
			d.ID = fmt.Sprintf("%s_%s", d.Source, d.Hash())
		}
		if d.CreatedAt.IsZero() {
			d.CreatedAt = time.Now()
		}
		res, err := stmt.ExecContext(ctx, d.ID, d.Source, d.Lang, d.Group, d.Title, d.Text, d.Hash(), d.CreatedAt)
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

// CountDocuments vrátí počet dokumentů podle filtru.
func (s *DB) CountDocuments(source string) (int64, error) {
	var count int64
	query := "SELECT COUNT(*) FROM documents"
	args := []any{}
	if source != "" {
		query += " WHERE source = ?"
		args = append(args, source)
	}
	err := s.db.QueryRow(query, args...).Scan(&count)
	return count, err
}

// ExportJSONL exportuje nezaembeddované dokumenty do JSONL souboru.
// Označí je jako embedded=1 po exportu.
func (s *DB) ExportJSONL(path string, batchSize int) (int, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, fmt.Errorf("create jsonl: %w", err)
	}
	defer f.Close()

	rows, err := s.db.Query(`
		SELECT id, source, lang, grp, title, text, created_at
		FROM documents
		WHERE embedded = 0
		ORDER BY created_at
		LIMIT ?
	`, batchSize)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	enc := json.NewEncoder(f)
	count := 0
	var ids []string

	for rows.Next() {
		var d Document
		var createdStr string
		if err := rows.Scan(&d.ID, &d.Source, &d.Lang, &d.Group, &d.Title, &d.Text, &createdStr); err != nil {
			return count, err
		}
		d.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)

		if err := enc.Encode(d); err != nil {
			return count, err
		}
		ids = append(ids, d.ID)
		count++
	}

	if len(ids) > 0 {
		// Označ jako embedded
		placeholders := make([]string, len(ids))
		args := make([]any, len(ids))
		for i, id := range ids {
			placeholders[i] = "?"
			args[i] = id
		}
		// SQLite nepodporuje IN s placeholders přímo, použij transakci
		tx, err := s.db.Begin()
		if err != nil {
			return count, err
		}
		stmt, err := tx.Prepare("UPDATE documents SET embedded = 1 WHERE id = ?")
		if err != nil {
			tx.Rollback()
			return count, err
		}
		defer stmt.Close()
		for _, id := range ids {
			stmt.Exec(id)
		}
		if err := tx.Commit(); err != nil {
			return count, err
		}
	}

	return count, nil
}

// SetState uloží stav downloadu (pro resume).
func (s *DB) SetState(key, value string) error {
	_, err := s.db.Exec(
		"INSERT OR REPLACE INTO download_state (key, value) VALUES (?, ?)",
		key, value,
	)
	return err
}

// GetState načte uložený stav.
func (s *DB) GetState(key string) (string, bool) {
	var value string
	err := s.db.QueryRow(
		"SELECT value FROM download_state WHERE key = ?", key,
	).Scan(&value)
	if err != nil {
		return "", false
	}
	return value, true
}

// Close zavře databázi.
func (s *DB) Close() error {
	return s.db.Close()
}
