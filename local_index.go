package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"unicode"

	"github.com/kljensen/snowball/russian"
	_ "modernc.org/sqlite"
)

type localWorkspace struct {
	path     string
	root     *os.Root
	db       *sql.DB
	mu       sync.Mutex
	snapshot *localSnapshot
}

func openLocalWorkspace(ctx context.Context, directory string) (*localWorkspace, error) {
	git := exec.CommandContext(ctx, "git", "-C", directory, "rev-parse", "--show-toplevel", "--absolute-git-dir")
	out, err := git.Output()
	if err != nil {
		return nil, fmt.Errorf("resolve local Git worktree: %w", err)
	}
	paths := strings.Split(strings.ReplaceAll(strings.TrimSpace(string(out)), "\r\n", "\n"), "\n")
	if len(paths) != 2 {
		return nil, errors.New("expected Git worktree and index directories")
	}
	projectDir, gitDir := filepath.FromSlash(paths[0]), filepath.FromSlash(paths[1])
	root, err := os.OpenRoot(projectDir)
	if err != nil {
		return nil, err
	}
	if _, err := root.Stat(localDir + "/project.json"); err != nil {
		root.Close()
		return nil, fmt.Errorf("%s: %w", localDir+"/project.json", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(gitDir, "contextshare-index-v1.sqlite"))
	if err != nil {
		root.Close()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	w := &localWorkspace{path: projectDir, root: root, db: db}
	for _, statement := range []string{
		"PRAGMA busy_timeout=5000",
		"PRAGMA journal_mode=WAL",
		"CREATE TABLE IF NOT EXISTS index_state (singleton INTEGER PRIMARY KEY CHECK (singleton=1), source_hash TEXT NOT NULL)",
		"CREATE VIRTUAL TABLE IF NOT EXISTS node_search USING fts5(id UNINDEXED, title, body)",
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			w.close()
			return nil, err
		}
	}
	return w, nil
}

func (w *localWorkspace) close() {
	w.db.Close()
	w.root.Close()
}

func (w *localWorkspace) load() (*localSnapshot, error) {
	files, hash, err := readLocalFiles(w.root)
	if err != nil {
		return nil, err
	}
	if w.snapshot != nil && hash == w.snapshot.hash {
		return w.snapshot, nil
	}
	snapshot, err := buildLocalSnapshot(w.root, files, hash)
	if err != nil {
		return nil, err
	}
	w.snapshot = snapshot
	return snapshot, nil
}

// One worktree can be used by several Codex processes. SQLite serializes
// rebuilding the derived index and the check/rename of source records.
func (w *localWorkspace) writeLock(ctx context.Context, change func(*sql.Conn) error) error {
	conn, err := w.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	if err := change(conn); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}

func (w *localWorkspace) index(ctx context.Context, data *localSnapshot) error {
	const currentVersion = "1:"
	key := currentVersion + data.hash
	var stored string
	err := w.db.QueryRowContext(ctx, "SELECT source_hash FROM index_state WHERE singleton=1").Scan(&stored)
	if err == nil && stored == key {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return w.writeLock(ctx, func(conn *sql.Conn) error {
		err := conn.QueryRowContext(ctx, "SELECT source_hash FROM index_state WHERE singleton=1").Scan(&stored)
		if err == nil && stored == key {
			return nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := conn.ExecContext(ctx, "DELETE FROM node_search"); err != nil {
			return err
		}
		insert, err := conn.PrepareContext(ctx, "INSERT INTO node_search(id,title,body) VALUES(?,?,?)")
		if err != nil {
			return err
		}
		defer insert.Close()
		for _, note := range data.notes {
			n := note.Node
			if _, err := insert.ExecContext(ctx, n.ID, strings.Join(localTerms(n.Title), " "), strings.Join(localTerms(n.Body), " ")); err != nil {
				return err
			}
		}
		_, err = conn.ExecContext(ctx, "INSERT INTO index_state(singleton,source_hash) VALUES(1,?) ON CONFLICT(singleton) DO UPDATE SET source_hash=excluded.source_hash", key)
		return err
	})
}

var localTermAliases = map[string]string{
	russian.Stem("шарнир", false):         "hinge",
	russian.Stem("черновик", false):       "draft",
	russian.Stem("офлайн", false):         "offline",
	russian.Stem("оффлайн", false):        "offline",
	russian.Stem("автосохранение", false): "autosave",
}

func localTerms(input string) []string {
	words := strings.FieldsFunc(strings.ToLower(input), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := words[:0]
	for _, word := range words {
		word = strings.ReplaceAll(word, "ё", "е")
		switch word {
		case "и", "в", "на", "по", "для", "а", "из", "с", "к", "о", "у", "не", "как", "почему", "the", "a", "of":
			continue
		}
		if strings.IndexFunc(word, func(r rune) bool { return unicode.Is(unicode.Cyrillic, r) }) >= 0 {
			word = russian.Stem(word, false)
		}
		if alias := localTermAliases[word]; alias != "" {
			word = alias
		}
		if word != "" {
			out = append(out, word)
		}
	}
	return out
}
