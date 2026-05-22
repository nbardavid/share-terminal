// Package update: self-update of the `control` binary from GitHub Releases.
//
// Flow:
//  1. GET https://api.github.com/repos/.../releases/latest to find the tag.
//  2. Resolve the asset matching `control-$GOOS-$GOARCH` plus checksums.txt.
//  3. Download the asset into a temp file next to the current binary
//     (same filesystem, so atomic rename is guaranteed).
//  4. Verify the SHA-256 against the matching line in checksums.txt.
//  5. chmod +x then os.Rename over the current binary.
//
// On Linux and macOS, replacing a running binary is fine: the kernel
// keeps the original inode while the process runs, and the next
// invocation uses the new file.
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	repo          = "nbardavid/share-terminal"
	releasesAPI   = "https://api.github.com/repos/" + repo + "/releases/latest"
	httpTimeout   = 30 * time.Second
	downloadLimit = 100 << 20 // 100 MiB cap per binary — generous for Go binaries
)

type release struct {
	TagName string  `json:"tag_name"`
	Name    string  `json:"name"`
	HTMLURL string  `json:"html_url"`
	Assets  []asset `json:"assets"`
}

type asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// CheckLatest returns the tag of the latest published release.
func CheckLatest(ctx context.Context) (string, error) {
	r, err := fetchLatest(ctx)
	if err != nil {
		return "", err
	}
	return r.TagName, nil
}

// Run fetches the latest release, downloads the binary matching
// GOOS/GOARCH, verifies its SHA-256, and replaces the current binary.
// currentVersion is only used to print "already up to date" when it matches.
func Run(ctx context.Context, currentVersion string) error {
	r, err := fetchLatest(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("Current version: %s\nLatest release:  %s\n", currentVersion, r.TagName)

	if currentVersion != "" && currentVersion != "dev" && currentVersion == r.TagName {
		fmt.Println("Already up to date.")
		return nil
	}

	target := fmt.Sprintf("control-%s-%s", runtime.GOOS, runtime.GOARCH)
	var binURL, sumURL string
	for _, a := range r.Assets {
		switch a.Name {
		case target:
			binURL = a.URL
		case "checksums.txt":
			sumURL = a.URL
		}
	}
	if binURL == "" {
		return fmt.Errorf("no binary for %s/%s in release %s", runtime.GOOS, runtime.GOARCH, r.TagName)
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("os.Executable: %w", err)
	}
	// Resolve symlinks so we replace the real binary, not something like
	// /usr/local/bin/control → ~/.local/bin/control.
	if real, err := filepath.EvalSymlinks(self); err == nil {
		self = real
	}

	// Temp file in the SAME directory as the current binary: atomic
	// rename is guaranteed (same filesystem).
	tmp, err := os.CreateTemp(filepath.Dir(self), ".control-update-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	// Cleanup on failure.
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	defer cleanup()

	fmt.Printf("-> Downloading %s...\n", target)
	if err := downloadTo(ctx, binURL, tmp); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if sumURL != "" {
		if err := verifyChecksum(ctx, tmpPath, target, sumURL); err != nil {
			return fmt.Errorf("SHA-256 verification failed: %w", err)
		}
		fmt.Println("SHA-256 verified.")
	} else {
		fmt.Println("No checksums.txt in release — skipping SHA-256 verification.")
	}

	// Permissions on the new binary = same as the current one (usually
	// 0755). Important on installs without exec bit by default.
	if info, err := os.Stat(self); err == nil {
		_ = os.Chmod(tmpPath, info.Mode())
	} else {
		_ = os.Chmod(tmpPath, 0o755)
	}

	if err := os.Rename(tmpPath, self); err != nil {
		return fmt.Errorf("replace binary (%s): %w", self, err)
	}
	// The temp file has been moved, nothing left to clean up.
	cleanup = func() {}

	fmt.Printf("Updated to %s\n", r.TagName)
	return nil
}

func fetchLatest(ctx context.Context) (*release, error) {
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", releasesAPI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "control-self-update")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.New("no published release for this repo (404)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github API status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var r release
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("parse release JSON: %w", err)
	}
	return &r, nil
}

func downloadTo(ctx context.Context, url string, w io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, httpTimeout*4) // a binary can be slower
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "control-self-update")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download status %d for %s", resp.StatusCode, url)
	}
	_, err = io.Copy(w, io.LimitReader(resp.Body, downloadLimit))
	return err
}

func verifyChecksum(ctx context.Context, file, name, sumURL string) error {
	// Fetch checksums.txt.
	var sumBuf strings.Builder
	if err := downloadTo(ctx, sumURL, stringWriter{&sumBuf}); err != nil {
		return err
	}

	// Look for the "<sha256>  <name>" line.
	var expected string
	for _, line := range strings.Split(sumBuf.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == name {
			expected = fields[0]
			break
		}
	}
	if expected == "" {
		return fmt.Errorf("no checksum for %s in checksums.txt", name)
	}

	// Hash of the downloaded file.
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))

	if got != expected {
		return fmt.Errorf("SHA-256 mismatch (expected %s, got %s)", expected, got)
	}
	return nil
}

// stringWriter adapts a strings.Builder to io.Writer for downloadTo.
type stringWriter struct{ b *strings.Builder }

func (s stringWriter) Write(p []byte) (int, error) { return s.b.Write(p) }
