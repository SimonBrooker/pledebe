package plex

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeTree creates each path relative to root, making parent directories.
func writeTree(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
}

// Container images disagree on layout, which is why discovery scans for the
// database rather than joining known path fragments. Both real layouts must
// resolve to the correct config root.
func TestDiscoverLayouts(t *testing.T) {
	cases := []struct {
		name       string
		dbPath     string
		configRoot string
	}{
		{
			// hotio: /config IS the "Plex Media Server" directory.
			name:       "flat (hotio)",
			dbPath:     "Plug-in Support/Databases/" + DatabaseName,
			configRoot: ".",
		},
		{
			// lsio / plexinc nest it three levels deeper.
			name:       "nested (lsio, plexinc)",
			dbPath:     "Library/Application Support/Plex Media Server/Plug-in Support/Databases/" + DatabaseName,
			configRoot: "Library/Application Support/Plex Media Server",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, tc.dbPath)

			in, err := Discover(root)
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}

			wantDB := filepath.Join(root, filepath.FromSlash(tc.dbPath))
			if in.Database != wantDB {
				t.Errorf("Database = %q, want %q", in.Database, wantDB)
			}

			wantRoot := filepath.Clean(filepath.Join(root, filepath.FromSlash(tc.configRoot)))
			if in.ConfigRoot != wantRoot {
				t.Errorf("ConfigRoot = %q, want %q", in.ConfigRoot, wantRoot)
			}
		})
	}
}

func TestDiscoverFindsCompanions(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root,
		"Plug-in Support/Databases/"+DatabaseName,
		"Plug-in Support/Databases/com.plexapp.plugins.library.blobs.db",
		"Logs/Plex Media Server.log",
	)

	in, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if in.BlobsDatabase == "" {
		t.Error("BlobsDatabase not found")
	}
	if in.LogDir == "" {
		t.Error("LogDir not found")
	}
}

func TestDiscoverNotFound(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, "Logs/Plex Media Server.log")

	if _, err := Discover(root); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// A missing or non-directory root must be skipped, not fatal — pledebe is
// given several candidate roots and only some will exist.
func TestDiscoverSkipsBadRoots(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, "Plug-in Support/Databases/"+DatabaseName)

	in, err := Discover("", filepath.Join(root, "nope"), root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if in.Database == "" {
		t.Error("expected the valid root to be used")
	}
}
