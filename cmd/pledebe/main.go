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
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/SimonBrooker/pledebe/internal/health"
	"github.com/SimonBrooker/pledebe/internal/notify"
	"github.com/SimonBrooker/pledebe/internal/plex"
	"github.com/SimonBrooker/pledebe/internal/store"
	"github.com/SimonBrooker/pledebe/internal/web"
)

// version is set at build time from the git tag. "dev" means an untagged
// local build, which links to the releases list rather than a release page.
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
		retain = flag.Duration("retain", envDuration("PLEDEBE_RETAIN", 14*24*time.Hour),
			"how long to keep fine-grained samples; daily history is kept forever")
		deepHour = flag.Int("deep-hour", envInt("DEEP_CHECK_HOUR", 4),
			"hour of day (0-23) to run the integrity deep check")
		once = flag.Bool("once", false, "collect once, print a report, and exit")
		deep = flag.Bool("deep", false,
			"run one integrity deep check now, print the result, and exit")
		asJSON = flag.Bool("json", false, "with -once, emit JSON")
	)
	flag.Parse()

	install, db, err := setup(*configRoot, *sqliteDir, *backupDir, *dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pledebe: %v\n", err)
		os.Exit(1)
	}

	if *deep {
		if err := runDeep(install, db, *dataDir); err != nil {
			fmt.Fprintf(os.Stderr, "pledebe: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *once {
		if err := runOnce(install, db, *asJSON); err != nil {
			fmt.Fprintf(os.Stderr, "pledebe: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := serve(install, db, *dataDir, *addr, *interval, *retain, *deepHour); err != nil {
		fmt.Fprintf(os.Stderr, "pledebe: %v\n", err)
		os.Exit(1)
	}
}

func setup(configRoot, sqliteDir, backupDir, dataDir string) (*plex.Install, *plex.SQLite, error) {
	install, err := plex.Discover(configRoot)
	if err != nil {
		return nil, nil, err
	}
	install.BackupDirOverride = backupDir

	db, err := plex.FindSQLite(sqliteDir)
	if err == nil {
		return install, db, nil
	}

	// Nothing mounted. If the operator opted into Docker extraction, fetch it
	// ourselves rather than making them run `docker cp` by hand.
	if os.Getenv("PLEX_SQLITE_SOURCE") != "docker" {
		return nil, nil, fmt.Errorf(
			"%w\n\nEither mount Plex's plexmediaserver directory at %s, or set "+
				"PLEX_SQLITE_SOURCE=docker (with PLEX_CONTAINER) and let pledebe "+
				"copy it out of the Plex container itself",
			err, sqliteDir)
	}

	db, err = extractSQLite(dataDir)
	if err != nil {
		return nil, nil, err
	}
	return install, db, nil
}

// extractSQLite copies Plex's install directory out of its container using the
// Docker API. Only GET requests are issued, so a socket proxy with POST
// disabled is enough -- it cannot stop, start or exec anything.
func extractSQLite(dataDir string) (*plex.SQLite, error) {
	container := envOr("PLEX_CONTAINER", "plex")
	host := envOr("DOCKER_HOST", "unix:///var/run/docker.sock")

	ex, err := plex.NewDockerExtractor(host, container)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	log.Printf("extracting Plex SQLite from container %q via %s", container, host)
	dir, err := ex.Extract(ctx, filepath.Join(dataDir, "plexbin"))
	if err != nil {
		return nil, fmt.Errorf("extracting Plex SQLite: %w", err)
	}
	log.Printf("extracted to %s", dir)

	return plex.FindSQLite(dir)
}

func runOnce(install *plex.Install, db *plex.SQLite, asJSON bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	m, err := install.Collect(ctx, db, time.Time{})
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
	interval, retain time.Duration, deepHour int) error {

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	st, err := store.Open(filepath.Join(dataDir, "history.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	deepFn := func(ctx context.Context) error {
		dc, err := install.RunDeepCheck(ctx, db, filepath.Join(dataDir, "scratch"))
		if dc != nil {
			if storeErr := st.InsertDeepCheck(dc); storeErr != nil {
				log.Printf("deep check: store failed: %v", storeErr)
			}
		}
		return err
	}

	auth := web.Auth{
		User:     os.Getenv("PLEDEBE_USER"),
		Password: os.Getenv("PLEDEBE_PASSWORD"),
	}

	// Validated before anything starts: a half-configured mailer should fail
	// here, naming what is missing, rather than appearing to work and never
	// sending.
	mail := emailConfig()
	if err := mail.Validate(); err != nil {
		return err
	}
	host := envOr("PLEDEBE_HOST", shortHostname())

	srv, err := web.New(install, db.BinaryPath, version, st, deepFn, auth, mail, host)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Collect immediately so the page has something to show, then on a ticker.
	go collectLoop(ctx, install, db, st, interval, retain, mail, host)
	go deepCheckLoop(ctx, install, db, st, filepath.Join(dataDir, "scratch"), deepHour)

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

	// Print the build identity first. A stale image was mistaken for a bug
	// three times during development, each time diagnosed by noticing that an
	// error message had the OLD wording. Saying which build is running turns
	// that into a glance.
	log.Printf("pledebe %s (built %s)", version, buildInfo())
	log.Printf("watching %s", install.Database)
	if note := startupPrefsWarning(install); note != "" {
		log.Printf("WARNING: %s", note)
	}
	log.Printf("status page on %s, collecting every %s", addr, interval)
	web.WarnIfExposed(addr, auth)

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Print("shut down cleanly")
	return nil
}

// deepCheckLoop runs one integrity verification per day at deepHour, and one
// shortly after startup if the most recent is older than a day.
//
// The snapshot costs a read lock and a file copy — 4s for a 1.1GB library — so
// it is safe alongside a running Plex, but it is still I/O, hence once daily
// and outside Plex's default Butler window where possible.
func deepCheckLoop(ctx context.Context, install *plex.Install, db *plex.SQLite,
	st *store.Store, scratchDir string, deepHour int) {

	run := func() {
		last, err := st.LatestDeepCheck()
		if err == nil && last != nil && time.Since(last.StartedAt) < 20*time.Hour {
			return
		}

		log.Print("deep check: starting snapshot")
		dc, err := install.RunDeepCheck(ctx, db, scratchDir)
		if err != nil {
			log.Printf("deep check: %v", err)
		} else if dc.IntegrityOK {
			log.Printf("deep check: integrity ok, %.0fs snapshot, %d MB reclaimable",
				dc.SnapshotSec, dc.ReclaimableBytes/(1<<20))
		} else {
			log.Print("deep check: INTEGRITY CHECK FAILED")
		}
		// Record failures too — "the check could not run" is information.
		if dc != nil {
			if err := st.InsertDeepCheck(dc); err != nil {
				log.Printf("deep check: store failed: %v", err)
			}
		}
	}

	// Give the first metric collection a moment rather than starting both at
	// once on a cold container.
	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
		run()
	}

	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if now.Hour() == deepHour {
				run()
			}
		}
	}
}

func collectLoop(ctx context.Context, install *plex.Install, db *plex.SQLite,
	st *store.Store, interval, retain time.Duration,
	mail notify.Config, host string) {

	// Slow-query lines are only counted once: each poll scans from where the
	// last one stopped. Zero on the first pass takes whatever the logs hold.
	var lastPoll time.Time

	collect := func() {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()

		m, err := install.Collect(callCtx, db, lastPoll)
		if err != nil {
			log.Printf("collect failed: %v", err)
			return
		}
		if err := st.Insert(m); err != nil {
			log.Printf("store failed: %v", err)
			return
		}
		lastPoll = m.CollectedAt
		if err := st.RollupDay(m.CollectedAt); err != nil {
			log.Printf("rollup failed: %v", err)
		}
		if err := st.PruneRaw(retain); err != nil {
			log.Printf("prune failed: %v", err)
		}

		// Evaluate against the latest deep check so integrity and search-index
		// problems are notified too, not only the cheap metrics.
		deep, _ := st.LatestDeepCheck()
		notifyChanges(st, mail, host, health.Evaluate(m, deep))
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
	for _, f := range health.Evaluate(m, nil) {
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

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("ignoring invalid %s=%q", key, v)
	}
	return fallback
}

// runDeep performs one deep check on demand and prints it.
//
// Records the result if the data directory is usable, so a manual run shows up
// on the status page — but a run without /data mounted still works and prints,
// because the common use is checking a server before committing to a
// deployment.
func runDeep(install *plex.Install, db *plex.SQLite, dataDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	var st *store.Store
	if err := os.MkdirAll(dataDir, 0o755); err == nil {
		if opened, err := store.Open(filepath.Join(dataDir, "history.db")); err == nil {
			st = opened
			defer st.Close()
		}
	}

	fmt.Printf("Snapshotting %s\n", install.Database)
	fmt.Println("Plex keeps running throughout; only a read lock is taken.")

	dc, err := install.RunDeepCheck(ctx, db, filepath.Join(dataDir, "scratch"))
	if dc == nil {
		return err
	}

	fmt.Println()
	if dc.Err != "" {
		fmt.Printf("Could not complete: %s\n", dc.Err)
	} else {
		fmt.Printf("integrity_check : %s\n", okOrFailed(dc.IntegrityOK))
		fmt.Printf("database        : %s\n", human(dc.DatabaseBytes))
		fmt.Printf("snapshot        : %s\n", human(dc.SnapshotBytes))
		fmt.Printf("reclaimable     : %s (exact, not the freelist estimate)\n", human(dc.ReclaimableBytes))
		fmt.Printf("snapshot took   : %.1fs\n", dc.SnapshotSec)
		fmt.Printf("check took      : %.1fs\n", dc.CheckSec)

		if dc.IntegrityDetail != "" {
			fmt.Printf("\nintegrity_check output:\n%s\n", dc.IntegrityDetail)
		}

		if len(dc.FTS) > 0 {
			fmt.Println("\nFull-text indexes (not covered by integrity_check):")
			fmt.Printf("  %-26s %-8s %10s %10s %10s\n", "index", "check", "indexed", "rows", "missing")
			for _, t := range dc.FTS {
				fmt.Printf("  %-26s %-8s %10d %10d %10d\n",
					t.Name, okOrFailed(t.IntegrityOK), t.IndexedDocs, t.SourceRows, t.MissingDocs())
			}
		}
	}

	fmt.Println("\nFindings")
	for _, f := range health.Evaluate(&plex.Metrics{}, dc) {
		if f.Level == health.LevelOK {
			continue // metrics are empty here; only the deep-check findings mean anything
		}
		fmt.Printf("  [%-7s] %s\n            %s\n", f.Level, f.Title, f.Detail)
	}

	if st != nil {
		if err := st.InsertDeepCheck(dc); err != nil {
			fmt.Fprintf(os.Stderr, "\nwarning: could not record the result: %v\n", err)
		} else {
			fmt.Println("\nRecorded — it will appear on the status page.")
		}
	}
	return err
}

func okOrFailed(ok bool) string {
	if ok {
		return "ok"
	}
	return "FAILED"
}

// buildInfo reports when the binary was built, read from the embedded VCS
// stamp Go records automatically.
func buildInfo() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.time" {
			return s.Value
		}
	}
	return "unknown"
}

// startupPrefsWarning surfaces an unreadable Preferences.xml at boot rather
// than leaving it to be inferred from a wrong finding later. Without that file
// pledebe cannot tell where backups are written or when Plex runs maintenance.
func startupPrefsWarning(install *plex.Install) string {
	if _, err := install.LoadPreferences(); err != nil {
		return err.Error()
	}
	return ""
}

// emailConfig reads notification settings from the environment.
//
// SMTP_PASSWORD is read here and passed straight to the mailer; like the Plex
// token it is never logged, stored or rendered.
func emailConfig() notify.Config {
	var to []string
	for _, addr := range strings.Split(os.Getenv("SMTP_TO"), ",") {
		if addr = strings.TrimSpace(addr); addr != "" {
			to = append(to, addr)
		}
	}
	return notify.Config{
		Host:     os.Getenv("SMTP_HOST"),
		Port:     envInt("SMTP_PORT", 587),
		User:     os.Getenv("SMTP_USER"),
		Password: os.Getenv("SMTP_PASSWORD"),
		From:     os.Getenv("SMTP_FROM"),
		To:       to,
		BaseURL:  os.Getenv("PLEDEBE_ORIGIN"),
	}
}

// notifyChanges emails about findings that have appeared or cleared since the
// last check.
//
// Errors are logged, never fatal: a mail server being down must not stop
// pledebe monitoring. And nothing is marked as notified unless the send
// succeeded, so a transient failure retries on the next poll rather than
// silently swallowing the only warning anyone would have received.
func notifyChanges(st *store.Store, cfg notify.Config, host string,
	findings []health.Finding) {

	if !cfg.Enabled() {
		return
	}

	already, err := st.NotifiedFindings()
	if err != nil {
		log.Printf("notify: reading state: %v", err)
		return
	}

	change := notify.Diff(findings, already)
	if !change.Any() {
		return
	}

	subject := notify.Subject(host, change)
	body := notify.Body(host, change, cfg.BaseURL)

	if err := cfg.Send(subject, body); err != nil {
		log.Printf("notify: send failed, will retry next check: %v", err)
		return
	}
	log.Printf("notify: emailed %d new, %d resolved", len(change.New), len(change.Recovered))

	for _, f := range change.New {
		if err := st.MarkNotified(f.Title, string(f.Level)); err != nil {
			log.Printf("notify: %v", err)
		}
	}
	for _, title := range change.Recovered {
		if err := st.ClearNotified(title); err != nil {
			log.Printf("notify: %v", err)
		}
	}
}

// shortHostname names this server in notifications.
//
// In a container os.Hostname() is the container ID, which tells the reader
// nothing, so PLEDEBE_HOST exists to override it with something recognisable.
func shortHostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "pledebe"
	}
	return h
}
