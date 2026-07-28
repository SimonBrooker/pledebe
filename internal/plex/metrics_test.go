package plex

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Regression test for two shipped bugs.
//
// v1 counted entries directly under "Crash Reports" and reported 171 PMS
// version directories as 171 crashes. v2 counted files one level down and would
// have reported zero regardless of how many crashes existed, because the real
// layout is version/COMPONENT/files.
func TestCollectCrashesCountsNestedFiles(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root,
		"Crash Reports/1.43.3.10828-00f62d37d/PLEX MEDIA SERVER/crash-1.dmp",
		"Crash Reports/1.43.3.10828-00f62d37d/PLEX MEDIA SERVER/crash-2.dmp",
		"Crash Reports/1.43.3.10828-00f62d37d/PLEX MEDIA SCANNER/crash-3.dmp",
		"Crash Reports/1.43.2.10687-563d026ea/PLEX TUNER SERVICE/crash-4.dmp",
	)

	in := &Install{ConfigRoot: root}
	m := &Metrics{}
	in.collectCrashes(m)

	if m.CrashReportCount != 4 {
		t.Errorf("CrashReportCount = %d, want 4 (not the 2 version dirs)", m.CrashReportCount)
	}
	if got := m.CrashesByComponent["PLEX MEDIA SERVER"]; got != 2 {
		t.Errorf("PLEX MEDIA SERVER = %d, want 2", got)
	}
	if got := m.CrashesByComponent["PLEX TUNER SERVICE"]; got != 1 {
		t.Errorf("PLEX TUNER SERVICE = %d, want 1", got)
	}
}

// Empty component directories are the normal, healthy case — a server with no
// crashes still has a directory per version.
func TestCollectCrashesEmptyComponents(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"PLEX MEDIA SERVER", "PLEX MEDIA SCANNER", "PLEX TUNER SERVICE"} {
		if err := os.MkdirAll(filepath.Join(root, "Crash Reports", "1.43.3.10828", d), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	in := &Install{ConfigRoot: root}
	m := &Metrics{}
	in.collectCrashes(m)

	if m.CrashReportCount != 0 {
		t.Errorf("CrashReportCount = %d, want 0", m.CrashReportCount)
	}
	if m.PMSVersion != "1.43.3.10828" {
		t.Errorf("PMSVersion = %q, want the version directory name", m.PMSVersion)
	}
}

// Regression test for the false alarm: backups were read from beside the
// database while PMS was writing them to ButlerDatabaseBackupPath, producing a
// confident "91 days old" on a server backing up nightly.
func TestCollectBackupsPrefersConfiguredPath(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, "Databases")
	current := filepath.Join(root, "configured-backups")

	writeTree(t, stale, DatabaseName+"-2026-04-28")
	writeTree(t, current, DatabaseName+"-2026-07-27")

	in := &Install{BackupDir: stale, BackupDirOverride: current}
	m := &Metrics{}
	in.collectBackups(m, &Preferences{DatabaseBackupPath: "/backup/Databases"})

	if m.BackupDirSearched != current {
		t.Errorf("searched %q, want the configured path %q", m.BackupDirSearched, current)
	}
	want := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	if !m.NewestBackupAt.Equal(want) {
		t.Errorf("NewestBackupAt = %v, want %v (the stale copy must not win)", m.NewestBackupAt, want)
	}
	if !m.BackupDirVisible {
		t.Error("BackupDirVisible = false, want true")
	}
}

// The critical distinction: PMS records its backup path in its own mount
// namespace. If pledebe cannot see it, that is "unknown", never "no backups".
func TestCollectBackupsInvisibleIsNotAFailure(t *testing.T) {
	in := &Install{BackupDir: filepath.Join(t.TempDir(), "does-not-exist")}
	m := &Metrics{}
	in.collectBackups(m, &Preferences{DatabaseBackupPath: "/backup/Databases"})

	if m.BackupDirVisible {
		t.Error("BackupDirVisible = true, want false")
	}
	if m.BackupCount != 0 {
		t.Errorf("BackupCount = %d, want 0", m.BackupCount)
	}
	if m.BackupDirExpected != "/backup/Databases" {
		t.Errorf("BackupDirExpected = %q, want the configured path so the UI can explain itself",
			m.BackupDirExpected)
	}
}

// Preferences.xml holds PlexOnlineToken. Parsing must keep only whitelisted
// attributes so a secret cannot reach a log, a diagnostic bundle, or the UI.
func TestLoadPreferencesIgnoresSecrets(t *testing.T) {
	root := t.TempDir()
	xml := `<?xml version="1.0" encoding="utf-8"?>
<Preferences PlexOnlineToken="SECRET-TOKEN-VALUE" ButlerDatabaseBackupPath="/backup/Databases" ButlerStartHour="2" ButlerEndHour="8" logDebug="1" />`
	if err := os.WriteFile(filepath.Join(root, "Preferences.xml"), []byte(xml), 0o644); err != nil {
		t.Fatal(err)
	}

	in := &Install{ConfigRoot: root}
	prefs, err := in.LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences: %v", err)
	}

	if prefs.DatabaseBackupPath != "/backup/Databases" {
		t.Errorf("DatabaseBackupPath = %q", prefs.DatabaseBackupPath)
	}
	if prefs.ButlerStartHour != "2" || prefs.ButlerEndHour != "8" {
		t.Errorf("Butler window = %q-%q, want 2-8", prefs.ButlerStartHour, prefs.ButlerEndHour)
	}
	if prefs.LogDebug != "1" {
		t.Errorf("LogDebug = %q, want 1", prefs.LogDebug)
	}
}

// A missing Preferences.xml is normal, not an error.
func TestLoadPreferencesMissingFile(t *testing.T) {
	in := &Install{ConfigRoot: t.TempDir()}
	prefs, err := in.LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences: %v", err)
	}
	if prefs.DatabaseBackupPath != "" {
		t.Error("expected zero Preferences")
	}
}
