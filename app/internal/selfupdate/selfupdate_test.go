package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newFakeGitHub serves a release with one linux/amd64 asset, its checksum, and
// records every path requested.
func newFakeGitHub(t *testing.T, tag string, binary []byte, corruptSum bool) (*httptest.Server, *[]string) {
	t.Helper()
	var paths []string
	name := AssetName(tag, "linux", "amd64")
	sum := sha256.Sum256(binary)
	sumHex := hex.EncodeToString(sum[:])
	if corruptSum {
		sumHex = strings.Repeat("0", 64)
	}
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch {
		case strings.HasPrefix(r.URL.Path, "/repos/test/repo/releases"):
			rel := release{TagName: tag, Assets: []asset{
				{Name: name, URL: base + "/dl/" + name, Size: int64(len(binary))},
				{Name: name + ".sha256", URL: base + "/dl/" + name + ".sha256"},
			}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(rel)
		case r.URL.Path == "/dl/"+name:
			_, _ = w.Write(binary)
		case r.URL.Path == "/dl/"+name+".sha256":
			fmt.Fprintf(w, "%s  %s\n", sumHex, name)
		default:
			http.NotFound(w, r)
		}
	}))
	base = srv.URL
	t.Cleanup(srv.Close)
	return srv, &paths
}

// setupExisting creates the "current" binary the update replaces.
func setupExisting(t *testing.T) (dir, exe string) {
	t.Helper()
	dir = t.TempDir()
	exe = filepath.Join(dir, "inceptionlabs2api")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, exe
}

func testOpts(srv *httptest.Server, exe string, exec func(string, []string) error) Options {
	return Options{
		Repo:    "test/repo",
		Exe:     exe,
		GOOS:    "linux",
		GOARCH:  "amd64",
		Client:  srv.Client(),
		APIBase: srv.URL,
		Logf:    func(string, ...any) {},
		Exec:    exec,
	}
}

// The happy path: the new bytes land on the executable path, the previous
// binary is kept as .old, and exec is asked to run the new file.
func TestUpdateInstallsVerifiedBinaryAndExecs(t *testing.T) {
	binary := []byte("new binary v1.2.0")
	srv, _ := newFakeGitHub(t, "v1.2.0", binary, false)
	dir, exe := setupExisting(t)

	var execPath string
	var execArgs []string
	res, err := Update(context.Background(), testOpts(srv, exe, func(p string, args []string) error {
		execPath, execArgs = p, args
		return nil
	}))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if res.Version != "v1.2.0" || res.Asset != "inceptionlabs2api_1.2.0_linux_amd64" || !res.Replaced {
		t.Fatalf("unexpected result: %+v", res)
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(binary) {
		t.Fatalf("installed bytes = %q, want the downloaded binary", got)
	}
	if old, err := os.ReadFile(exe + ".old"); err != nil || string(old) != "old binary" {
		t.Fatalf(".old backup = %q, %v; want the previous binary", old, err)
	}
	if execPath != exe {
		t.Fatalf("exec path = %q, want %q", execPath, exe)
	}
	_ = execArgs
	// No temp files left beside the binary.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".inceptionlabs2api-update-") {
			t.Fatalf("temporary file %q was not cleaned up", e.Name())
		}
	}
}

// A checksum mismatch must abort BEFORE anything on disk is touched: the
// running binary and its backup stay as they were.
func TestUpdateRejectsAChecksumMismatch(t *testing.T) {
	srv, _ := newFakeGitHub(t, "v1.2.0", []byte("tampered"), true)
	_, exe := setupExisting(t)

	_, err := Update(context.Background(), testOpts(srv, exe, func(string, []string) error {
		t.Fatal("exec ran despite a checksum mismatch")
		return nil
	}))
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error = %v, want a checksum mismatch", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old binary" {
		t.Fatalf("binary was modified after a failed verification: %q", got)
	}
	if _, err := os.Stat(exe + ".old"); !os.IsNotExist(err) {
		t.Fatal("a .old backup was created even though nothing was installed")
	}
}

// A failed exec must leave a runnable gateway behind: the new binary stays
// installed and the error says a restart is needed.
func TestUpdateReportsAFailedExecWithoutLosingTheBinary(t *testing.T) {
	binary := []byte("new binary")
	srv, _ := newFakeGitHub(t, "v1.2.0", binary, false)
	_, exe := setupExisting(t)

	res, err := Update(context.Background(), testOpts(srv, exe, func(string, []string) error {
		return fmt.Errorf("boom")
	}))
	if err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("error = %v, want a restart hint", err)
	}
	if res == nil || !res.Replaced {
		t.Fatalf("result = %+v, want Replaced=true", res)
	}
	if got, _ := os.ReadFile(exe); string(got) != string(binary) {
		t.Fatalf("installed bytes = %q", got)
	}
}

// A release without checksums is not trusted: there is nothing to verify
// against, so the update must refuse.
func TestUpdateRefusesAReleaseWithoutChecksums(t *testing.T) {
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(release{TagName: "v9.9.9", Assets: []asset{
			{Name: AssetName("v9.9.9", "linux", "amd64"), URL: base + "/bin"},
		}})
	}))
	t.Cleanup(srv.Close)
	base = srv.URL
	_, exe := setupExisting(t)

	_, err := Update(context.Background(), testOpts(srv, exe, func(string, []string) error {
		t.Fatal("exec ran without a checksum")
		return nil
	}))
	if err == nil || !strings.Contains(err.Error(), "no checksum") {
		t.Fatalf("error = %v, want a missing-checksum refusal", err)
	}
}

func TestUpdatePinsATag(t *testing.T) {
	binary := []byte("pinned")
	srv, paths := newFakeGitHub(t, "v1.0.0", binary, false)
	_, exe := setupExisting(t)
	opts := testOpts(srv, exe, func(string, []string) error { return nil })
	opts.Tag = "v1.0.0"

	if _, err := Update(context.Background(), opts); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(*paths) == 0 || (*paths)[0] != "/repos/test/repo/releases/tags/v1.0.0" {
		t.Fatalf("first request = %v, want the pinned-tag endpoint", *paths)
	}
}

func TestAssetName(t *testing.T) {
	cases := map[string]string{
		AssetName("v1.2.0", "linux", "amd64"):   "inceptionlabs2api_1.2.0_linux_amd64",
		AssetName("1.2.0", "darwin", "arm64"):   "inceptionlabs2api_1.2.0_darwin_arm64",
		AssetName("v1.2.0", "windows", "amd64"): "inceptionlabs2api_1.2.0_windows_amd64.exe",
	}
	// The expected name must match what the release workflow produces.
	for got, want := range cases {
		t.Run(want, func(t *testing.T) {
			if got != want {
				t.Fatalf("AssetName = %q, want %q", got, want)
			}
		})
	}
}

// A release that matches the running version must not re-download the same
// bytes just to exec them.
func TestUpdateIsANoOpOnTheSameVersion(t *testing.T) {
	srv, paths := newFakeGitHub(t, "v1.2.0", []byte("same"), false)
	_, exe := setupExisting(t)
	opts := testOpts(srv, exe, func(string, []string) error {
		t.Fatal("exec ran for an already-current version")
		return nil
	})
	opts.Current = "1.2.0"

	res, err := Update(context.Background(), opts)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if res.Replaced {
		t.Fatal("Replaced is true for a no-op update")
	}
	if got, _ := os.ReadFile(exe); string(got) != "old binary" {
		t.Fatalf("binary replaced on a no-op: %q", got)
	}
	for _, p := range *paths {
		if strings.HasPrefix(p, "/dl/") {
			t.Fatalf("downloaded %s for an already-current version", p)
		}
	}
}

// A dev build must always update, even when the tag is the same shape.
func TestSameVersion(t *testing.T) {
	cases := []struct {
		current, tag string
		want         bool
	}{
		{"1.2.0", "v1.2.0", true},
		{"v1.2.0", "v1.2.0", true},
		{"dev", "v1.2.0", false},
		{"none", "v1.2.0", false},
		{"1.1.0", "v1.2.0", false},
	}
	for _, tc := range cases {
		if got := sameVersion(tc.current, tc.tag); got != tc.want {
			t.Errorf("sameVersion(%q, %q) = %v, want %v", tc.current, tc.tag, got, tc.want)
		}
	}
}
