package plex

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// SQLiteBinaryName is the name of Plex's own SQLite build.
//
// Stock sqlite3 is not a substitute. Plex installs custom collations, and
// PRAGMA integrity_check compares index entries using them — stock sqlite3
// fails with "no such collation sequence". The schema also uses the spellfix1
// and rtree extensions, which stock builds do not carry.
const SQLiteBinaryName = "Plex SQLite"

// SQLite runs queries using Plex's SQLite binary.
type SQLite struct {
	// BinaryPath is the full path to the "Plex SQLite" executable.
	BinaryPath string

	// Timeout bounds a single query. Deep operations (VACUUM INTO on a large
	// library) need a longer timeout than the default; callers override it.
	Timeout time.Duration
}

// knownSQLiteDirs are locations seen in the wild, tried before falling back to
// a scan. Verified: plexinc and lsio use the first, hotio the second.
var knownSQLiteDirs = []string{
	"/usr/lib/plexmediaserver",
	"/app/bin/usr/lib/plexmediaserver",
	"/app/usr/lib/plexmediaserver",
}

// FindSQLite locates the Plex SQLite binary.
//
// searchRoots are directories to search if the binary is not in a known
// location — typically a directory the operator has mounted or into which the
// binary was extracted.
//
// Important: the binary is NOT self-contained. It expects siblings in its own
// directory, so whatever supplied it must have supplied the whole
// plexmediaserver directory (~218MB), not just the one file. Copying the file
// alone fails at runtime with "Failed to start child process".
func FindSQLite(searchRoots ...string) (*SQLite, error) {
	for _, dir := range knownSQLiteDirs {
		candidate := filepath.Join(dir, SQLiteBinaryName)
		if isExecutable(candidate) {
			return newSQLite(candidate), nil
		}
	}

	for _, root := range searchRoots {
		if root == "" {
			continue
		}
		// Allow pointing directly at the binary as well as at a directory.
		if isExecutable(root) && filepath.Base(root) == SQLiteBinaryName {
			return newSQLite(root), nil
		}
		if found := scanForBinary(root); found != "" {
			return newSQLite(found), nil
		}
	}

	return nil, fmt.Errorf("%q not found (searched known locations and %s)",
		SQLiteBinaryName, strings.Join(searchRoots, ", "))
}

func newSQLite(path string) *SQLite {
	return &SQLite{BinaryPath: path, Timeout: 30 * time.Second}
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

func scanForBinary(root string) string {
	var found string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() && d.Name() == SQLiteBinaryName {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// Query runs sql against the database at dbPath and returns raw stdout.
//
// Plex SQLite writes errors to stdout rather than stderr in some cases and
// exits non-zero for others, so callers must not assume a nil error means the
// output is data. Use the typed helpers below where possible.
func (s *SQLite) Query(ctx context.Context, dbPath, sql string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, s.BinaryPath, dbPath, sql)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))

	if err != nil {
		return text, fmt.Errorf("query failed: %w (output: %.200s)", err, text)
	}
	return text, nil
}

// QueryInt runs sql and parses a single integer result.
func (s *SQLite) QueryInt(ctx context.Context, dbPath, sql string) (int64, error) {
	out, err := s.Query(ctx, dbPath, sql)
	if err != nil {
		return 0, err
	}
	line := strings.TrimSpace(firstLine(out))
	n, parseErr := strconv.ParseInt(line, 10, 64)
	if parseErr != nil {
		return 0, fmt.Errorf("expected an integer, got %.100q", out)
	}
	return n, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
