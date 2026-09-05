package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed index.html styles.css app.js service-worker.js favicon.ico favicon.svg check-circle.svg fonts/*.woff2 emoji/*
var staticFiles embed.FS

//go:embed migrations/*.sql
var migrationFiles embed.FS

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	db, err := openDatabase(cfg.DatabasePath, cfg.MaxDatabaseBytes, cfg.MaxJournalBytes)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

	if err := applyMigrations(db, migrationFiles); err != nil {
		log.Fatalf("apply migrations: %v", err)
	}

	app, err := newApp(db, cfg, staticFiles, time.Now, rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	if err := app.cleanup(context.Background()); err != nil {
		log.Printf("startup cleanup failed: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go app.runCleanup(ctx, time.Hour)

	server := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           app.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("server shutdown: %v", err)
		}
	}()

	log.Printf("letsgoout listening on %s", cfg.ListenAddress)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

type config struct {
	ListenAddress    string
	PublicBaseURL    string
	DatabasePath     string
	GlobalDailyLimit int
	MaxDatabaseBytes int64
	MaxJournalBytes  int64
}

func loadConfig() (config, error) {
	globalDailyLimit, err := envPositiveInt("GLOBAL_DAILY_LIMIT", 500)
	if err != nil {
		return config{}, err
	}
	maxDatabaseBytes, err := envPositiveInt64("MAX_DATABASE_BYTES", defaultMaxDatabaseBytes)
	if err != nil {
		return config{}, err
	}
	maxJournalBytes, err := envPositiveInt64("MAX_JOURNAL_BYTES", defaultMaxJournalBytes)
	if err != nil {
		return config{}, err
	}
	cfg := config{
		ListenAddress:    envString("LISTEN_ADDRESS", "0.0.0.0:8080"),
		PublicBaseURL:    strings.TrimRight(envString("PUBLIC_BASE_URL", "https://letsgoout.paulenriquez.com"), "/"),
		DatabasePath:     envString("DATABASE_PATH", "/data/letsgoout.db"),
		GlobalDailyLimit: globalDailyLimit,
		MaxDatabaseBytes: maxDatabaseBytes,
		MaxJournalBytes:  maxJournalBytes,
	}
	if cfg.PublicBaseURL == "" || cfg.DatabasePath == "" {
		return config{}, fmt.Errorf("PUBLIC_BASE_URL and DATABASE_PATH must not be empty")
	}
	return cfg, nil
}

func envString(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envPositiveInt(name string, fallback int) (int, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return n, nil
}

func envPositiveInt64(name string, fallback int64) (int64, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return n, nil
}

const (
	defaultMaxDatabaseBytes int64 = 256 << 20
	defaultMaxJournalBytes  int64 = 8 << 20
)

func openDatabase(path string, maxDatabaseBytes, maxJournalBytes int64) (*sql.DB, error) {
	if maxDatabaseBytes < 1 {
		return nil, errors.New("maximum database size must be positive")
	}
	if maxJournalBytes < 1 {
		return nil, errors.New("maximum journal size must be positive")
	}
	bootstrap, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	bootstrap.SetMaxOpenConns(1)
	var pageSize, pageCount int64
	if err := bootstrap.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err == nil {
		err = bootstrap.QueryRow(`PRAGMA page_count`).Scan(&pageCount)
	}
	if closeErr := bootstrap.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, fmt.Errorf("inspect database size: %w", err)
	}
	maxPageCount := maxDatabaseBytes / pageSize
	if maxPageCount < 1 {
		return nil, fmt.Errorf("MAX_DATABASE_BYTES must be at least one SQLite page (%d bytes)", pageSize)
	}
	if pageCount > maxPageCount {
		return nil, fmt.Errorf("database already uses %d bytes, exceeding MAX_DATABASE_BYTES=%d; raise the limit or VACUUM the database offline", pageCount*pageSize, maxDatabaseBytes)
	}

	dsn := fmt.Sprintf("%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)&_pragma=max_page_count(%d)&_pragma=journal_size_limit(%d)", path, maxPageCount, maxJournalBytes)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	var appliedMaxPageCount, appliedJournalLimit int64
	if err := db.QueryRowContext(ctx, `PRAGMA max_page_count`).Scan(&appliedMaxPageCount); err != nil {
		db.Close()
		return nil, fmt.Errorf("verify database page limit: %w", err)
	}
	if appliedMaxPageCount != maxPageCount {
		db.Close()
		return nil, fmt.Errorf("database page limit is %d pages, want %d", appliedMaxPageCount, maxPageCount)
	}
	if err := db.QueryRowContext(ctx, `PRAGMA journal_size_limit`).Scan(&appliedJournalLimit); err != nil {
		db.Close()
		return nil, fmt.Errorf("verify database journal limit: %w", err)
	}
	if appliedJournalLimit != maxJournalBytes {
		db.Close()
		return nil, fmt.Errorf("database journal limit is %d bytes, want %d", appliedJournalLimit, maxJournalBytes)
	}
	return db, nil
}

func applyMigrations(db *sql.DB, migrations fs.FS) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	entries, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	for _, name := range entries {
		base := strings.TrimSuffix(strings.TrimPrefix(name, "migrations/"), ".sql")
		versionText := strings.SplitN(base, "_", 2)[0]
		version, err := strconv.Atoi(versionText)
		if err != nil {
			return fmt.Errorf("invalid migration name %q", name)
		}
		var exists int
		err = db.QueryRow(`SELECT 1 FROM schema_migrations WHERE version = ?`, version).Scan(&exists)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		body, err := fs.ReadFile(migrations, name)
		if err != nil {
			return err
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err = tx.Exec(string(body)); err == nil {
			_, err = tx.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`, version, time.Now().Unix())
		}
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
