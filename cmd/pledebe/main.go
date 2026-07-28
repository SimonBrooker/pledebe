// Command pledebe monitors the health of a Plex Media Server database.
//
// Read-only by construction: it opens nothing for writing under the Plex
// config directory, has no Docker socket, and contains no repair code path.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/SimonBrooker/pledebe/internal/health"
	"github.com/SimonBrooker/pledebe/internal/plex"
	"github.com/SimonBrooker/pledebe/internal/store"
	"github.com/SimonBrooker/pledebe/internal/web"
)

var version = "dev"

func main() {
	var (
		configRoot = flag.String("config", envOr("PLEX_CONFIG", "/plexconfig"),
			"directory to scan for the Plex database")
		sqliteDir = flag.String("sqlite", envOr("PLEX_SQLITE_DIR", "/plexbin"),
			"directory containing the Plex SQLite binary (and its siblings)")
		backupDir = flag.String("backups", envOr("PLEX_BACKUP_DIR", ""),
			"where PMS's configured backup path is mounted for pledebe to read")
		dataDir = flag.String("data", envOr("PLEDEBE_DATA", "/data"),
			"directory for pledebe's own history database")
		addr = flag.String("addr", envOr("PLEDEBE_ADDR", ":8080"),
			"listen address for the status page")
		interval = flag.Duration("interval", envDuration("SCAN_INTERVAL", 15*time.Minute),
			"how often to collect metrics")
		retain = flag.Duration("retain", envDuration("PLEDEBE_RETAIN", 90*24*time.Hour),
			"how long to keep history")
		once   = flag.Bool("once", false, "collect once, print a report, and exit")
		asJSON = flag.Bool("json", false, "with -once, emit JSON")
	)
	flag.Parse()

	install, db, err := setup(*configRoot, *sqliteDir, *backupDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pledebe: %v\n", err)
		os.Exit(1)
	}

	if *once {
		if err := runOnce(install, db, *asJSON); err != nil {
			fmt.Fprintf(os.Stderr, "pledebe: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := serve(install, db, *dataDir, *addr, *interval, *retain); err != nil {
		fmt.Fprintf(os.Stderr, "pledebe: %v\n", err)
		os.Exit(1)
	}
}

func setup(configRoot, sqliteDir, backupDir string) (*plex.Install, *plex.SQLite, error) {
	install, err := plex.Discover(configRoot)
	if err != nil {
		return nil, nil, err
	}
	install.BackupDirOverride = backupDir

	db, err := plex.FindSQLite(sqliteDir)
	if err != nil {
		return nil, nil, err
	}
	return install, db, nil
}

func runOnce(install *plex.Install, db *plex.SQLite, asJSON bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	m, err := install.Collect(ctx, db)
	if err != nil {
		return err
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(m)
	}
	report(install, db, m)
	return nil
}

func serve(install *plex.Install, db *plex.SQLite, dataDir, addr string,
	interval, retain time.Duration) error {

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	st, err := store.Open(filepath.Join(dataDir, "history.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	srv, err := web.New(install, st)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Collect immediately so the page has something to show, then on a ticker.
	go collectLoop(ctx, install, db, st, interval, retain)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Printf("pledebe %s watching %s", version, install.Database)
	log.Printf("status page on %s, collecting every %s", addr, interval)

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Print("shut down cleanly")
	return nil
}

func collectLoop(ctx context.Context, install *plex.Install, db *plex.SQLite,
	st *store.Store, interval, retain time.Duration) {

	collect := func() {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()

		m, err := install.Collect(callCtx, db)
		if err != nil {
			log.Printf("collect failed: %v", err)
			return
		}
		if err := st.Insert(m); err != nil {
			log.Printf("store failed: %v", err)
			return
		}
		if err := st.Prune(retain); err != nil {
			log.Printf("prune failed: %v", err)
		}
	}

	collect()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			collect()
		}
	}
}

func report(in *plex.Install, db *plex.SQLite, m *plex.Metrics) {
	fmt.Println("Discovery")
	fmt.Printf("  config root   : %s\n", in.ConfigRoot)
	fmt.Printf("  database      : %s\n", in.Database)
	fmt.Printf("  logs          : %s\n", orNone(in.LogDir))
	fmt.Printf("  Plex SQLite   : %s\n", db.BinaryPath)

	fmt.Println("\nSize")
	fmt.Printf("  library.db    : %s\n", human(m.DatabaseBytes))
	fmt.Printf("  wal / shm     : %s / %s\n", human(m.WALBytes), human(m.SHMBytes))
	fmt.Printf("  blobs.db      : %s\n", human(m.BlobsBytes))
	fmt.Printf("  pages         : %d x %d bytes\n", m.PageCount, m.PageSize)
	fmt.Printf("  free pages    : %d (%.1f%%)\n", m.FreelistCount, m.FreeRatio()*100)
	fmt.Printf("  reclaimable   : at least %s (floor, not an estimate)\n", human(m.FreelistBytes))

	fmt.Println("\nEnvironment")
	if m.PMSVersion != "" {
		fmt.Printf("  PMS version   : %s (since %s)\n",
			m.PMSVersion, m.VersionSeenAt.Format("2006-01-02"))
	}
	fmt.Printf("  crash files   : %d total, %d in the last 14 days\n",
		m.CrashReportCount, m.RecentCrashCount)
	for component, n := range m.CrashesByComponent {
		fmt.Printf("    %-22s %d\n", component, n)
	}
	fmt.Printf("  volume free   : %s\n", human(m.VolumeFreeBytes))

	fmt.Println("\nFindings")
	for _, f := range health.Evaluate(m) {
		fmt.Printf("  [%-7s] %s\n", f.Level, f.Title)
		fmt.Printf("            %s\n", f.Detail)
	}
}

func orNone(s string) string {
	if s == "" {
		return "(not found)"
	}
	return s
}

func human(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		log.Printf("ignoring invalid %s=%q", key, v)
	}
	return fallback
}
