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
	if cfg.DisableRateLimits {
		log.Print("WARNING: request rate limits are disabled")
	}

	db, err := openDatabase(cfg.DatabasePath)
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
	ListenAddress     string
	PublicBaseURL     string
	DatabasePath      string
	RateLimitHMACKey  []byte
	CreateHourlyLimit int
	CreateDailyLimit  int
	GlobalDailyLimit  int
	ViewMinuteLimit   int
	AcceptMinuteLimit int
	DisableRateLimits bool
}

func loadConfig() (config, error) {
	disableRateLimits, err := envBool("DISABLE_RATE_LIMITS", false)
	if err != nil {
		return config{}, err
	}
	cfg := config{
		ListenAddress:     envString("LISTEN_ADDRESS", "0.0.0.0:8080"),
		PublicBaseURL:     strings.TrimRight(envString("PUBLIC_BASE_URL", "https://letsgoout.paulenriquez.com"), "/"),
		DatabasePath:      envString("DATABASE_PATH", "/data/letsgoout.db"),
		CreateHourlyLimit: envInt("CREATE_HOURLY_LIMIT", 5),
		CreateDailyLimit:  envInt("CREATE_DAILY_LIMIT", 20),
		GlobalDailyLimit:  envInt("GLOBAL_DAILY_LIMIT", 500),
		ViewMinuteLimit:   envInt("VIEW_MINUTE_LIMIT", 60),
		AcceptMinuteLimit: envInt("ACCEPT_MINUTE_LIMIT", 10),
		DisableRateLimits: disableRateLimits,
	}
	key := os.Getenv("RATE_LIMIT_HMAC_KEY")
	if !cfg.DisableRateLimits && len(key) < 32 {
		return config{}, fmt.Errorf("RATE_LIMIT_HMAC_KEY must contain at least 32 bytes")
	}
	cfg.RateLimitHMACKey = []byte(key)
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

func envBool(name string, fallback bool) (bool, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", name)
	}
	return parsed, nil
}

func envInt(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 {
		log.Fatalf("%s must be a positive integer", name)
	}
	return n
}

func openDatabase(path string) (*sql.DB, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"
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
