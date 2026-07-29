package plex

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tar comes from another container's filesystem. It is untrusted input, and
// these are the ways an archive escapes its destination.
func TestSafeJoinRejectsEscapes(t *testing.T) {
	root := t.TempDir()

	bad := []string{
		"../outside",
		"../../etc/passwd",
		"plexmediaserver/../../escape",
		"/etc/passwd",
		"/absolute",
	}
	for _, name := range bad {
		if _, err := safeJoin(root, name); err == nil {
			t.Errorf("safeJoin accepted %q, which escapes the destination", name)
		}
	}

	good := []string{
		"plexmediaserver/Plex SQLite",
		"plexmediaserver/lib/libsomething.so",
		"./plexmediaserver/nested/deep/file",
	}
	for _, name := range good {
		if _, err := safeJoin(root, name); err != nil {
			t.Errorf("safeJoin rejected legitimate path %q: %v", name, err)
		}
	}
}

func tarball(t *testing.T, entries func(*tar.Writer)) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	entries(tw)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func TestUntarExtractsAndPreservesExecutableBit(t *testing.T) {
	dest := t.TempDir()

	buf := tarball(t, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{
			Name: "plexmediaserver/", Typeflag: tar.TypeDir, Mode: 0o755,
		})
		body := []byte("#!/bin/sh\n")
		_ = tw.WriteHeader(&tar.Header{
			Name: "plexmediaserver/" + SQLiteBinaryName, Typeflag: tar.TypeReg,
			Mode: 0o755, Size: int64(len(body)),
		})
		_, _ = tw.Write(body)
	})

	if err := untarInto(buf, dest); err != nil {
		t.Fatalf("untarInto: %v", err)
	}

	// FindSQLite must locate it, since that is how extraction is verified.
	sqlite, err := FindSQLite(dest)
	if err != nil {
		t.Fatalf("extracted tree has no usable binary: %v", err)
	}
	info, err := os.Stat(sqlite.BinaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Error("executable bit lost; the binary would not run")
	}
}

// A traversal entry must abort the extraction, not be silently skipped.
func TestUntarRefusesTraversal(t *testing.T) {
	dest := t.TempDir()

	buf := tarball(t, func(tw *tar.Writer) {
		body := []byte("owned")
		_ = tw.WriteHeader(&tar.Header{
			Name: "../escaped.txt", Typeflag: tar.TypeReg,
			Mode: 0o644, Size: int64(len(body)),
		})
		_, _ = tw.Write(body)
	})

	if err := untarInto(buf, dest); err == nil {
		t.Fatal("traversal entry was accepted")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "escaped.txt")); err == nil {
		t.Fatal("a file was written outside the destination")
	}
}

// A symlink pointing out of the tree would let a malicious image redirect later
// reads at the host filesystem.
func TestUntarRefusesEscapingSymlink(t *testing.T) {
	dest := t.TempDir()

	buf := tarball(t, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{
			Name: "plexmediaserver/evil", Typeflag: tar.TypeSymlink,
			Linkname: "../../../../etc/passwd", Mode: 0o777,
		})
	})

	err := untarInto(buf, dest)
	if err == nil {
		t.Fatal("escaping symlink was accepted")
	}
	if !strings.Contains(err.Error(), "escapes destination") {
		t.Errorf("error = %v, want it to name the reason", err)
	}
}

// Relative links inside the tree are legitimate and must survive.
func TestUntarAllowsInternalSymlink(t *testing.T) {
	dest := t.TempDir()

	buf := tarball(t, func(tw *tar.Writer) {
		body := []byte("x")
		_ = tw.WriteHeader(&tar.Header{
			Name: "plexmediaserver/lib/real.so", Typeflag: tar.TypeReg,
			Mode: 0o644, Size: int64(len(body)),
		})
		_, _ = tw.Write(body)
		_ = tw.WriteHeader(&tar.Header{
			Name: "plexmediaserver/lib/link.so", Typeflag: tar.TypeSymlink,
			Linkname: "real.so", Mode: 0o777,
		})
	})

	if err := untarInto(buf, dest); err != nil {
		t.Fatalf("legitimate internal symlink rejected: %v", err)
	}
}

// Device nodes and FIFOs have no business in a Plex install directory.
func TestUntarSkipsSpecialFiles(t *testing.T) {
	dest := t.TempDir()

	buf := tarball(t, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{
			Name: "plexmediaserver/dev", Typeflag: tar.TypeChar, Mode: 0o666,
		})
	})

	if err := untarInto(buf, dest); err != nil {
		t.Fatalf("untarInto: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "plexmediaserver", "dev")); err == nil {
		t.Error("a character device was created")
	}
}

func TestNewDockerExtractorValidates(t *testing.T) {
	if _, err := NewDockerExtractor("unix:///var/run/docker.sock", ""); err == nil {
		t.Error("expected an error with no container name")
	}
	if _, err := NewDockerExtractor("ftp://nope", "plex"); err == nil {
		t.Error("expected an error for an unsupported scheme")
	}
	if _, err := NewDockerExtractor("", "plex"); err != nil {
		t.Errorf("default socket path should work: %v", err)
	}
}

// A cached extraction is reused only when the image matches; a Plex update must
// force a fresh copy.
func TestCachedExtractionKeyedOnImage(t *testing.T) {
	dest := t.TempDir()
	dir := filepath.Join(dest, "plexmediaserver")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, SQLiteBinaryName), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeExtractionMarker(dest, "sha256:aaa", "/usr/lib/plexmediaserver"); err != nil {
		t.Fatal(err)
	}

	if _, ok := cachedExtraction(dest, "sha256:aaa"); !ok {
		t.Error("matching image should reuse the extraction")
	}
	if _, ok := cachedExtraction(dest, "sha256:bbb"); ok {
		t.Error("a different image must force re-extraction")
	}
}
