// Package selfupdate replaces the running gateway binary with a newer release
// from GitHub and execs it.
//
// It exists because the gateway is deployed as a single static binary (plus a
// .env) that nobody wants to re-download by hand: `inceptionlabs2api -update`
// fetches the newest GitHub Release asset for this OS/architecture, checks it
// against the release's published .sha256, writes it beside the running binary
// under a new name, and execs it. The process image and PID never change, so
// whatever supervisor started it (a shell, systemd, a manually started
// process) does not need to know anything happened.
//
// The update is deliberately atomic and reversible:
//
//  1. download to a temporary file next to the target (same filesystem, so the
//     final rename is atomic) while streaming the sha256;
//  2. refuse to install a checksum that does not match the release's .sha256 —
//     a truncated download or a hijacked asset never reaches the rename;
//  3. on Unix, rename the old binary to "<exe>.old" and the new one into place,
//     then exec the new path. If the exec fails, the old binary is restored.
//
// Windows has no atomic replace over a running executable, so the update fails
// there with a clear message rather than leaving a half-installed binary.
package selfupdate

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

// DefaultRepo is the GitHub repository whose releases carry the binaries. The
// asset naming it relies on is the release workflow's:
//
//	inceptionlabs2api_<version>_<goos>_<goarch>[.exe]
//
// and the checksum file beside it has the same name plus ".sha256".
const DefaultRepo = "6zev/inceptionlabs2api"

// maxDownload bounds an asset. The binaries are ~7 MB; the cap only stops a
// hostile or broken response from filling the disk.
const maxDownload = 128 << 20

// maxReleaseBytes bounds the GitHub API response body.
const maxReleaseBytes = 8 << 20

// Options configures one update.
type Options struct {
	// Repo is "owner/name"; empty means DefaultRepo.
	Repo string
	// Asset is an explicit asset name; empty derives it from GOOS/GOARCH.
	Asset string
	// Tag pins a release tag ("v1.2.0"); empty means the newest release.
	Tag string
	// Exe is the binary to replace; empty means the running executable.
	Exe string
	// GOOS/GOARCH default to the running platform.
	GOOS, GOARCH string
	// Client is the HTTP client; nil means a 120 s default.
	Client *http.Client
	// APIBase is overridable for tests; empty means api.github.com.
	APIBase string
	// Logf defaults to a no-op (main logs the printed summary instead).
	Logf func(format string, args ...any)
	// Exec replaces the running process; nil means execSelf. Tests inject it.
	Exec func(exe string, args []string) error
	// ARGV is the argument list the new image gets, including argv[0]'s
	// extra arguments; nil means "just the program path".
	ARGV []string
	// Current is the running version ("1.2.0", or "dev" for a local build).
	// When it equals the release tag the update is a no-op: re-downloading
	// an identical binary and re-execing for nothing is worse than saying so.
	Current string
}

// Result is what an update did.
type Result struct {
	Version  string // tag, e.g. "v1.2.0"
	Asset    string
	Path     string // where the new binary was installed
	Bytes    int64
	Checksum string // hex sha256 actually written
	Replaced bool   // false when the target was already this release
}

// release is the subset of the GitHub release object this package reads.
type release struct {
	TagName string  `json:"tag_name"`
	Assets  []asset `json:"assets"`
}

type asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// AssetName derives the release asset name for a platform.
func AssetName(version, goos, goarch string) string {
	name := fmt.Sprintf("inceptionlabs2api_%s_%s_%s", strings.TrimPrefix(version, "v"), goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// Update downloads the newest (or pinned) release and installs it. It execs
// the new binary on success, so a nil error means control never came back;
// a non-nil error always means the running binary was left untouched (or
// restored).
func Update(ctx context.Context, opts Options) (*Result, error) {
	repo := opts.Repo
	if repo == "" {
		repo = DefaultRepo
	}
	goos, goarch := opts.GOOS, opts.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	if goos == "windows" {
		// Nothing atomic can replace a running .exe.
		return nil, errors.New("self-update is not supported on Windows; download the new release by hand")
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	exe := opts.Exe
	if exe == "" {
		var err error
		exe, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("resolve running executable: %w", err)
		}
	}
	exe, err := filepath.EvalSymlinks(exe)
	if err != nil {
		// A binary we cannot resolve is still the one to replace; keep going
		// with the path the OS reported.
		logf("selfupdate: eval symlinks %s: %v", exe, err)
	}

	rel, err := fetchRelease(ctx, client, opts.APIBase, repo, opts.Tag)
	if err != nil {
		return nil, err
	}
	name := opts.Asset
	if name == "" {
		name = AssetName(rel.TagName, goos, goarch)
	}
	a, sum, err := findAsset(rel, name)
	if err != nil {
		return nil, err
	}

	res := &Result{Version: rel.TagName, Asset: a.Name, Path: exe}
	if sameVersion(opts.Current, rel.TagName) {
		logf("selfupdate: %s is already the newest release", rel.TagName)
		return res, nil
	}
	if err := install(ctx, client, a, sum, exe, res, logf); err != nil {
		return nil, err
	}

	exec := opts.Exec
	if exec == nil {
		exec = execSelf
	}
	logf("selfupdate: exec %s %v", exe, opts.ARGV)
	if err := exec(exe, opts.ARGV); err != nil {
		// The binary on disk is new but nothing is running it: say so rather
		// than pretending a restart happened. The next start picks it up.
		return res, fmt.Errorf("binary replaced with %s, but exec failed (restart the gateway to run it): %w", rel.TagName, err)
	}
	return res, nil
}

// sameVersion compares a build's version string with a release tag. A local
// "dev" build never matches, so a developer's binary always updates.
func sameVersion(current, tag string) bool {
	current = strings.TrimPrefix(strings.TrimSpace(current), "v")
	tag = strings.TrimPrefix(strings.TrimSpace(tag), "v")
	return current != "" && current != "dev" && current == tag
}

// fetchRelease returns the pinned tag or the newest non-prerelease release.
func fetchRelease(ctx context.Context, c *http.Client, apiBase, repo, tag string) (*release, error) {
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	url := fmt.Sprintf("%s/repos/%s/releases/latest", strings.TrimRight(apiBase, "/"), repo)
	if tag != "" {
		url = fmt.Sprintf("%s/repos/%s/releases/tags/%s", strings.TrimRight(apiBase, "/"), repo, tag)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "inceptionlabs2api-selfupdate")
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch release: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReleaseBytes))
	if err != nil {
		return nil, fmt.Errorf("read release: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch release: GitHub answered HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var rel release
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, fmt.Errorf("decode release: %w", err)
	}
	if rel.TagName == "" {
		return nil, errors.New("release has no tag_name")
	}
	return &rel, nil
}

// findAsset returns the binary asset and its published checksum.
func findAsset(rel *release, name string) (asset, string, error) {
	var bin, sum *asset
	for i := range rel.Assets {
		a := &rel.Assets[i]
		switch a.Name {
		case name:
			bin = a
		case name + ".sha256":
			sum = a
		}
	}
	if bin == nil {
		return asset{}, "", fmt.Errorf("release %s has no asset %q", rel.TagName, name)
	}
	if sum == nil {
		// Refusing an unverifiable binary is the whole point; a release
		// without checksums is a broken release, not something to trust.
		return asset{}, "", fmt.Errorf("release %s has no checksum %q", rel.TagName, name+".sha256")
	}
	return *bin, sum.URL, nil
}

// install downloads the asset, verifies it, and atomically moves it into
// place. The running binary is renamed to "<exe>.old" first so a failed exec
// can restore it.
func install(ctx context.Context, c *http.Client, a asset, sumURL, exe string, res *Result, logf func(string, ...any)) error {
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".inceptionlabs2api-update-*")
	if err != nil {
		return fmt.Errorf("create temporary binary beside %s: %w", exe, err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	want, err := fetchChecksum(ctx, c, sumURL)
	if err != nil {
		return err
	}
	res.Checksum, res.Bytes, err = download(ctx, c, a.URL, tmp, want)
	if err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close downloaded binary: %w", err)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return fmt.Errorf("chmod downloaded binary: %w", err)
	}

	backup := exe + ".old"
	if err := os.Rename(exe, backup); err != nil {
		return fmt.Errorf("move current binary aside: %w", err)
	}
	if err := os.Rename(tmpName, exe); err != nil {
		// Put the old binary back before reporting: a failed update must not
		// leave the deployment without a runnable gateway.
		if rerr := os.Rename(backup, exe); rerr != nil {
			return fmt.Errorf("install new binary: %w (and restoring %s failed: %v)", err, backup, rerr)
		}
		return fmt.Errorf("install new binary: %w", err)
	}
	res.Replaced = true
	logf("selfupdate: installed %s (%d bytes) at %s; previous kept at %s", a.Name, res.Bytes, exe, backup)
	return nil
}

// fetchChecksum reads the published "<hex>  <filename>" checksum file.
func fetchChecksum(ctx context.Context, c *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch checksum: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", fmt.Errorf("read checksum: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch checksum: HTTP %d", resp.StatusCode)
	}
	fields := strings.Fields(string(body))
	if len(fields) == 0 || len(fields[0]) != 64 {
		return "", fmt.Errorf("checksum file is not a sha256 digest: %q", strings.TrimSpace(string(body)))
	}
	if _, err := hex.DecodeString(fields[0]); err != nil {
		return "", fmt.Errorf("checksum is not hex: %w", err)
	}
	return strings.ToLower(fields[0]), nil
}

// download streams the asset into dst while hashing it, and fails on a
// checksum mismatch, an empty body, or a body above maxDownload.
func download(ctx context.Context, c *http.Client, url string, dst *os.File, want string) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", "inceptionlabs2api-selfupdate")
	resp, err := c.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("download binary: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("download binary: HTTP %d", resp.StatusCode)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return "", 0, fmt.Errorf("download binary: %w", err)
	}
	if n > maxDownload {
		return "", 0, fmt.Errorf("download binary: body exceeds %d bytes", maxDownload)
	}
	if n == 0 {
		return "", 0, errors.New("download binary: empty body")
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return got, n, fmt.Errorf("checksum mismatch for download: got %s, release says %s", got, want)
	}
	return got, n, nil
}
