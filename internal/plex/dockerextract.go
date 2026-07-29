package plex

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Extracting Plex's SQLite build automatically, so the operator does not have
// to run `docker cp` by hand before anything works.
//
// This uses ONLY GET requests: /containers/{id}/json to identify the image, and
// /containers/{id}/archive to stream the directory out. `docker cp` is exactly
// that archive endpoint, which is why a socket proxy with POST disabled is
// sufficient — it cannot stop, start or exec anything.
//
// It is opt-in. The default deployment has no socket at all.

// dockerAPIVersion is pinned low enough to work with any daemon this is likely
// to meet, and with socket proxies that rewrite paths.
const dockerAPIVersion = "v1.41"

// maxExtractBytes caps a extraction. The real directory is ~218 MB; this leaves
// room while refusing to fill a disk if pointed at something unexpected.
const maxExtractBytes = 1 << 30

// candidatePlexDirs are tried in order against the archive endpoint. Each is a
// real location observed in a published Plex image.
var candidatePlexDirs = []string{
	"/usr/lib/plexmediaserver",         // plexinc, linuxserver, binhex
	"/app/bin/usr/lib/plexmediaserver", // hotio
	"/app/usr/lib/plexmediaserver",
}

// DockerExtractor pulls the Plex install directory out of a running container.
type DockerExtractor struct {
	// Host is a Docker endpoint: unix:///var/run/docker.sock, or
	// tcp://socket-proxy:2375 when using a proxy.
	Host string

	// Container is the Plex container's name or id.
	Container string

	client *http.Client
}

// NewDockerExtractor builds a client for the given endpoint.
func NewDockerExtractor(host, container string) (*DockerExtractor, error) {
	if container == "" {
		return nil, fmt.Errorf("no Plex container name given")
	}
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}

	client, err := dockerClient(host)
	if err != nil {
		return nil, err
	}
	return &DockerExtractor{Host: host, Container: container, client: client}, nil
}

func dockerClient(host string) (*http.Client, error) {
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("invalid docker host %q: %w", host, err)
	}

	switch u.Scheme {
	case "unix":
		return &http.Client{
			Timeout: 10 * time.Minute, // the archive stream is ~218 MB
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", u.Path)
				},
			},
		}, nil
	case "tcp", "http":
		return &http.Client{Timeout: 10 * time.Minute}, nil
	default:
		return nil, fmt.Errorf("unsupported docker host scheme %q", u.Scheme)
	}
}

// baseURL renders the endpoint for an HTTP request.
func (d *DockerExtractor) baseURL() string {
	u, err := url.Parse(d.Host)
	if err != nil || u.Scheme == "unix" {
		// Any host works for a unix socket; the dialer ignores it.
		return "http://docker/" + dockerAPIVersion
	}
	return "http://" + u.Host + "/" + dockerAPIVersion
}

// imageID identifies the image the container is running, used to decide whether
// a cached extraction is still current.
func (d *DockerExtractor) imageID(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		d.baseURL()+"/containers/"+url.PathEscape(d.Container)+"/json", nil)
	if err != nil {
		return "", err
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("docker api unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("container %q not found", d.Container)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("docker api returned %s inspecting %q", resp.Status, d.Container)
	}

	var body struct {
		Image string `json:"Image"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("decoding container info: %w", err)
	}
	return body.Image, nil
}

// Extract copies Plex's install directory into destDir and returns the
// directory containing the SQLite binary.
//
// If a previous extraction is present and came from the same image, it is
// reused: re-copying 218 MB on every restart would be pointless.
func (d *DockerExtractor) Extract(ctx context.Context, destDir string) (string, error) {
	image, err := d.imageID(ctx)
	if err != nil {
		return "", err
	}

	if dir, ok := cachedExtraction(destDir, image); ok {
		return dir, nil
	}

	// A partial extraction from an interrupted run must not be mistaken for a
	// good one.
	_ = os.RemoveAll(destDir)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("preparing %s: %w", destDir, err)
	}

	var lastErr error
	for _, candidate := range candidatePlexDirs {
		err := d.extractPath(ctx, candidate, destDir)
		if err != nil {
			lastErr = err
			continue
		}
		sqlite, err := FindSQLite(destDir)
		if err != nil {
			lastErr = fmt.Errorf("%s extracted but contains no %q", candidate, SQLiteBinaryName)
			continue
		}
		if err := writeExtractionMarker(destDir, image, candidate); err != nil {
			return "", err
		}
		return filepath.Dir(sqlite.BinaryPath), nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no Plex install directory found in container %q", d.Container)
	}
	return "", lastErr
}

func (d *DockerExtractor) extractPath(ctx context.Context, srcPath, destDir string) error {
	endpoint := d.baseURL() + "/containers/" + url.PathEscape(d.Container) +
		"/archive?path=" + url.QueryEscape(srcPath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("docker api unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: docker api returned %s", srcPath, resp.Status)
	}
	return untarInto(resp.Body, destDir)
}

// untarInto extracts a tar stream, refusing anything that would write outside
// destDir.
//
// The stream comes from another container's filesystem, so it is untrusted
// input: entry names could contain "..", be absolute, or be symlinks pointing
// at the host. Each is rejected rather than sanitised, because a Plex install
// directory legitimately contains none of them.
func untarInto(r io.Reader, destDir string) error {
	root, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}

	tr := tar.NewReader(io.LimitReader(r, maxExtractBytes))
	var written int64

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading archive: %w", err)
		}

		target, err := safeJoin(root, hdr.Name)
		if err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}

		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC,
				os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			n, err := io.Copy(f, tr)
			f.Close()
			if err != nil {
				return err
			}
			written += n
			if written > maxExtractBytes {
				return fmt.Errorf("archive exceeds %d bytes", maxExtractBytes)
			}

		case tar.TypeSymlink, tar.TypeLink:
			// A symlink whose target escapes the destination would let a
			// malicious image redirect later reads. Plex's install directory
			// contains only relative internal links.
			if _, err := safeJoin(filepath.Dir(target), hdr.Linkname); err != nil {
				return fmt.Errorf("refusing link %q -> %q: escapes destination",
					hdr.Name, hdr.Linkname)
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}

		default:
			// Devices, FIFOs and sockets have no business here.
			continue
		}
	}
}

// safeJoin resolves name against root and fails if the result escapes it.
func safeJoin(root, name string) (string, error) {
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("refusing absolute path in archive: %q", name)
	}
	target := filepath.Join(root, filepath.FromSlash(name))

	// filepath.Join cleans "..", so compare the result rather than scanning the
	// input for patterns — that catches encodings a substring check would miss.
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("refusing path outside destination: %q", name)
	}
	return target, nil
}

// extractionMarker records what produced a cached extraction.
type extractionMarker struct {
	Image     string    `json:"image"`
	SourceDir string    `json:"source_dir"`
	At        time.Time `json:"at"`
}

const markerName = ".pledebe-extraction.json"

func cachedExtraction(destDir, image string) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(destDir, markerName))
	if err != nil {
		return "", false
	}
	var m extractionMarker
	if err := json.Unmarshal(raw, &m); err != nil || m.Image != image {
		return "", false // different image: Plex was updated, re-extract
	}
	sqlite, err := FindSQLite(destDir)
	if err != nil {
		return "", false
	}
	return filepath.Dir(sqlite.BinaryPath), true
}

func writeExtractionMarker(destDir, image, sourceDir string) error {
	raw, err := json.Marshal(extractionMarker{
		Image: image, SourceDir: sourceDir, At: time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(destDir, markerName), raw, 0o644)
}
