package pluginrt

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// goos/goarch isolate runtime.GOOS/GOARCH so tests can pin table lookups:
// tests may override goosPinned/goarchPinned (restoring them afterwards)
// to exercise other platforms' asset/asset-name branches.
var (
	goosPinned   = runtime.GOOS
	goarchPinned = runtime.GOARCH
)

func goos() string   { return goosPinned }
func goarch() string { return goarchPinned }

// Runtime downloads are pinned to exact versions with SHA-256 digests taken
// from the projects' published checksum manifests (uv: sha256.sum; pinchtab:
// checksums.txt) and re-verified locally at download time. Upgrading a
// runtime means updating these tables.

const (
	uvVersion   = "0.12.5"
	pinchtabVer = "v0.15.1"
)

// Release bases stay pinned to the projects' GitHub downloads; they are vars
// only so tests can repoint them at a local server and exercise the
// download → verify → extract path without the network.
var (
	uvReleaseBase = "https://github.com/astral-sh/uv/releases/download/" + uvVersion
	ptReleaseBase = "https://github.com/pinchtab/pinchtab/releases/download/" + pinchtabVer
)

// uvChecksums maps GOOS-GOARCH to the sha256 of uv's release archive.
var uvChecksums = map[string]string{
	"darwin-arm64":  "5bb0e5fe008a773c3dbcb97ff79cd89e1241464fe9d2f986d52ad8f1b037bd62", // uv-aarch64-apple-darwin.tar.gz
	"darwin-amd64":  "b3b2137477cf96c9686ebfb71524614cec780c673fd73e59bce099aef02e70e8", // uv-x86_64-apple-darwin.tar.gz
	"linux-arm64":   "9bf43b4d1a07665bf64d4c4e710930b382321a785e0eb10aac07f46471f86a31", // uv-aarch64-unknown-linux-gnu.tar.gz
	"linux-amd64":   "68a509da24b06b4223a1c0175fb5eb5bc79342b76cbeff0cfe51ac3f5b17b6b2", // uv-x86_64-unknown-linux-gnu.tar.gz
	"windows-amd64": "4c4d49d8738847d9b71ba319e49a5688c93eac0fe6204b1df24e98528dddf39a", // uv-x86_64-pc-windows-msvc.zip
	"windows-arm64": "724279317fee6e5fa8ad1908e4eba2bbe764ef1ece5b3f4597927b62b1fe562a", // uv-aarch64-pc-windows-msvc.zip
}

// pinchtabChecksums maps GOOS-GOARCH to the sha256 of pinchtab's raw binary.
var pinchtabChecksums = map[string]string{
	"darwin-arm64":  "0d4df1655e1115c1d45b86d2c3c3ef9212b496195219e3415551c57dffb3e4d9",
	"darwin-amd64":  "29f9a0ebf5ddead810bed5ad42a9a97e540e3a3339a9413a67f6f6911d0be364",
	"linux-arm64":   "f2b0bfdf76d5e55ccc6a4d7e08bb934feffe930af6cede7a7403d046f2a47730",
	"linux-amd64":   "3f5ceaaf172e8c8c0276a0c8efa4009cb2484d60422e044a5fb21cf0577f893c",
	"windows-amd64": "6335dd1f5bc94ffc27930c901a4d5e7f87cc09e1ccacb1d8b03a98facf0cd15e",
	"windows-arm64": "ba8296be3f5487dd571f69a9b96bbbca10c6f55c6ab83b38ac179d83fdbc82a6",
}

// platformChecksum looks up a GOOS-GOARCH digest in one of the pin tables.
func platformChecksum(table map[string]string, what string) (string, error) {
	key := goos() + "-" + goarch()
	sum, ok := table[key]
	if !ok {
		return "", fmt.Errorf("unsupported platform %s for %s (no pinned download)", key, what)
	}
	return sum, nil
}

// uvAssetNames returns the release archive name for the current platform.
func uvAssetNames() (string, error) {
	arch := map[string]string{"arm64": "aarch64", "amd64": "x86_64"}[goarch()]
	osName := map[string]string{"darwin": "apple-darwin", "linux": "unknown-linux-gnu", "windows": "pc-windows-msvc"}[goos()]
	if arch == "" || osName == "" {
		return "", fmt.Errorf("unsupported platform %s-%s for uv", goos(), goarch())
	}
	if goos() == "windows" {
		return fmt.Sprintf("uv-%s-%s.zip", arch, osName), nil
	}
	return fmt.Sprintf("uv-%s-%s.tar.gz", arch, osName), nil
}

// pinchtabAssetName returns the raw-binary asset name for the platform.
func pinchtabAssetName() (string, error) {
	name := fmt.Sprintf("pinchtab-%s-%s", goos(), goarch())
	if goos() == "windows" {
		name += ".exe"
	}
	if _, ok := pinchtabChecksums[goos()+"-"+goarch()]; !ok {
		return "", fmt.Errorf("unsupported platform %s-%s for pinchtab", goos(), goarch())
	}
	return name, nil
}

// fetchVerified downloads url into dest (atomic write via a temp file in the
// same directory), streaming through a sha256 that must match wantSHA.
func fetchVerified(ctx context.Context, url, wantSHA, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "woca-pluginrt")
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}

	// Random temp name: python plugins share runtimes/bin/<asset> dests, so
	// two concurrent installs must not interleave writes into the same
	// fixed .download file.
	fh, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".download-*")
	if err != nil {
		return err
	}
	tmp := fh.Name()
	hasher := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(fh, hasher), resp.Body)
	closeErr := fh.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if got != wantSHA {
		_ = os.Remove(tmp)
		return fmt.Errorf("checksum mismatch for %s: got %s want %s — refusing to install", filepath.Base(dest), got, wantSHA)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// extractArchiveMembers pulls the named members (by base name, at any archive
// depth — uv wraps its binaries in a uv-<triple>/ directory) out of a .tar.gz
// or .zip archive into destDir with 0755 permissions.
func extractArchiveMembers(archive, destDir string, want map[string]bool) error {
	if strings.HasSuffix(archive, ".zip") {
		return extractZipMembers(archive, destDir, want)
	}
	fh, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer fh.Close()
	gz, err := gzip.NewReader(fh)
	if err != nil {
		return fmt.Errorf("gunzip %s: %w", archive, err)
	}
	tr := tar.NewReader(gz)
	found := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar %s: %w", archive, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		base := filepath.Base(hdr.Name)
		if !want[base] || found[base] {
			continue
		}
		if err := writeMember(tr, filepath.Join(destDir, base)); err != nil {
			return err
		}
		found[base] = true
	}
	return checkFound(found, want)
}

func extractZipMembers(archive, destDir string, want map[string]bool) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("open zip %s: %w", archive, err)
	}
	defer zr.Close()
	found := map[string]bool{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		base := filepath.Base(f.Name)
		if !want[base] || found[base] {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeMember(rc, filepath.Join(destDir, base))
		rc.Close()
		if err != nil {
			return err
		}
		found[base] = true
	}
	return checkFound(found, want)
}

func writeMember(r io.Reader, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	fh, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	_, err = io.Copy(fh, r)
	if closeErr := fh.Close(); err == nil {
		err = closeErr
	}
	return err
}

func checkFound(found, want map[string]bool) error {
	for name := range want {
		if !found[name] && name != "uvx" { // uvx is optional (uv is the one we run)
			return fmt.Errorf("archive did not contain %q", name)
		}
	}
	return nil
}
