// Package update : self-update du binaire `control` depuis GitHub Releases.
//
// Le flow :
//  1. GET https://api.github.com/repos/.../releases/latest pour trouver le tag.
//  2. Récupère l'asset matching `control-$GOOS-$GOARCH` et le checksums.txt.
//  3. Télécharge l'asset dans un fichier temporaire à côté du binaire courant
//     (même filesystem, donc rename atomique garanti).
//  4. Vérifie le SHA-256 contre la ligne correspondante dans checksums.txt.
//  5. chmod +x puis os.Rename par-dessus le binaire courant.
//
// Sur Linux et macOS, remplacer un binaire en cours d'exécution est OK :
// le kernel garde l'inode original tant que le processus tourne, et la
// prochaine invocation utilise le nouveau fichier.
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
	downloadLimit = 100 << 20 // 100 MiB plafond par binaire — large pour Go binaries
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

// CheckLatest renvoie le tag de la dernière release publiée.
func CheckLatest(ctx context.Context) (string, error) {
	r, err := fetchLatest(ctx)
	if err != nil {
		return "", err
	}
	return r.TagName, nil
}

// Run récupère la dernière release, télécharge le binaire matching
// GOOS/GOARCH, vérifie son SHA-256, et remplace le binaire courant.
// currentVersion sert juste à afficher "tu es déjà à jour" si ça matche.
func Run(ctx context.Context, currentVersion string) error {
	r, err := fetchLatest(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("Version courante : %s\nDernière release : %s\n", currentVersion, r.TagName)

	if currentVersion != "" && currentVersion != "dev" && currentVersion == r.TagName {
		fmt.Println("✓ Déjà à jour.")
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
		return fmt.Errorf("aucun binaire pour %s/%s dans la release %s", runtime.GOOS, runtime.GOARCH, r.TagName)
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("os.Executable: %w", err)
	}
	// Suit les symlinks pour qu'on remplace bien le vrai binaire,
	// pas un raccourci genre /usr/local/bin/control → ~/.local/bin/control.
	if real, err := filepath.EvalSymlinks(self); err == nil {
		self = real
	}

	// Fichier temporaire dans le MÊME répertoire que le binaire courant :
	// rename atomique garanti (même filesystem).
	tmp, err := os.CreateTemp(filepath.Dir(self), ".control-update-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	// Nettoyage en cas d'échec.
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	defer cleanup()

	fmt.Printf("→ Téléchargement de %s...\n", target)
	if err := downloadTo(ctx, binURL, tmp); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if sumURL != "" {
		if err := verifyChecksum(ctx, tmpPath, target, sumURL); err != nil {
			return fmt.Errorf("vérification SHA-256 échouée : %w", err)
		}
		fmt.Println("✓ SHA-256 vérifié.")
	} else {
		fmt.Println("⚠  Pas de checksums.txt dans la release — vérification SHA-256 sautée.")
	}

	// Permissions du nouveau binaire = mêmes que celles du courant
	// (généralement 0755). Important sur des installs sans exec bit par défaut.
	if info, err := os.Stat(self); err == nil {
		_ = os.Chmod(tmpPath, info.Mode())
	} else {
		_ = os.Chmod(tmpPath, 0o755)
	}

	if err := os.Rename(tmpPath, self); err != nil {
		return fmt.Errorf("remplacement du binaire (%s) : %w", self, err)
	}
	// Le fichier temp a été déplacé, plus rien à nettoyer.
	cleanup = func() {}

	fmt.Printf("✓ Mis à jour vers %s\n", r.TagName)
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
		return nil, errors.New("aucune release publiée pour ce repo (404)")
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
	ctx, cancel := context.WithTimeout(ctx, httpTimeout*4) // un binaire peut être plus lent
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
	// Récupère checksums.txt.
	var sumBuf strings.Builder
	if err := downloadTo(ctx, sumURL, stringWriter{&sumBuf}); err != nil {
		return err
	}

	// Cherche la ligne "<sha256>  <name>".
	var expected string
	for _, line := range strings.Split(sumBuf.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == name {
			expected = fields[0]
			break
		}
	}
	if expected == "" {
		return fmt.Errorf("pas de checksum pour %s dans checksums.txt", name)
	}

	// Hash du fichier téléchargé.
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
		return fmt.Errorf("SHA-256 mismatch (attendu %s, obtenu %s)", expected, got)
	}
	return nil
}

// stringWriter adapte un strings.Builder en io.Writer pour downloadTo.
type stringWriter struct{ b *strings.Builder }

func (s stringWriter) Write(p []byte) (int, error) { return s.b.Write(p) }
