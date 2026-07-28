// Command pledebe monitors the health of a Plex Media Server database.
//
// Milestone 1 is read-only by construction: it opens nothing for writing, has
// no Docker socket, and contains no repair code path.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/SimonBrooker/pledebe/internal/plex"
)

func main() {
	var (
		configRoot = flag.String("config", envOr("PLEX_CONFIG", "/plexconfig"),
			"directory to scan for the Plex database")
		sqliteDir = flag.String("sqlite", envOr("PLEX_SQLITE_DIR", "/plexbin"),
			"directory containing the Plex SQLite binary (and its siblings)")
		backupDir = flag.String("backups", envOr("PLEX_BACKUP_DIR", ""),
			"where PMS's configured backup path is mounted for pledebe to read")
		asJSON = flag.Bool("json", false, "emit JSON instead of a readable report")
	)
	flag.Parse()

	if err := run(*configRoot, *sqliteDir, *backupDir, *asJSON); err != nil {
		fmt.Fprintf(os.Stderr, "pledebe: %v\n", err)
		os.Exit(1)
	}
}

func run(configRoot, sqliteDir, backupDir string, asJSON bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	install, err := plex.Discover(configRoot)
	if err != nil {
		return err
	}
	install.BackupDirOverride = backupDir

	db, err := plex.FindSQLite(sqliteDir)
	if err != nil {
		return err
	}

	metrics, err := install.Collect(ctx, db)
	if err != nil {
		return err
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(metrics)
	}

	report(install, db, metrics)
	return nil
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

	fmt.Println("\nBackups")
	if m.BackupDirExpected != "" {
		fmt.Printf("  PMS writes to : %s (its own mount namespace)\n", m.BackupDirExpected)
	}
	switch {
	case m.BackupCount > 0:
		age := int(time.Since(m.NewestBackupAt).Hours() / 24)
		fmt.Printf("  searched      : %s\n", m.BackupDirSearched)
		fmt.Printf("  newest        : %s (%d days old)\n", m.NewestBackup, age)
		fmt.Printf("  count         : %d\n", m.BackupCount)

	case m.BackupDirExpected != "" && !m.BackupDirVisible:
		// Critical distinction: we cannot see the location, so we know NOTHING
		// about backup freshness. Saying "no backups" here would be a confident
		// false alarm -- it is exactly the bug this branch exists to prevent.
		fmt.Printf("  UNKNOWN -- cannot see %s from pledebe.\n", m.BackupDirExpected)
		fmt.Println("  Mount it and pass -backups (or PLEX_BACKUP_DIR) to check freshness.")

	default:
		fmt.Printf("  none found in %s\n", orNone(m.BackupDirSearched))
		fmt.Println("  (PMS may be writing elsewhere -- check ButlerDatabaseBackupPath)")
	}

	fmt.Println("\nEnvironment")
	if m.PMSVersion != "" {
		fmt.Printf("  PMS version   : %s (since %s)\n",
			m.PMSVersion, m.VersionSeenAt.Format("2006-01-02"))
	}
	fmt.Printf("  crash files   : %d total, %d in the last 14 days\n",
		m.CrashReportCount, m.RecentCrashCount)
	if !m.NewestCrashAt.IsZero() {
		fmt.Printf("  newest crash  : %s\n", m.NewestCrashAt.Format("2006-01-02"))
	}
	for component, n := range m.CrashesByComponent {
		fmt.Printf("    %-22s %d\n", component, n)
	}
	fmt.Printf("  volume free   : %s\n", human(m.VolumeFreeBytes))
	if m.VolumeFreeBytes > 0 && m.VolumeFreeBytes < m.DatabaseBytes {
		fmt.Println("  WARNING: less free space than the database size --")
		fmt.Println("           a snapshot or repair would not fit")
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
