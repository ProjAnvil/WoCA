package pluginrt

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// TestMain pins dead proxies for the whole test binary. Go's proxy env is
// read once per process, so setting it here (before any request) guarantees
// every non-loopback URL fails fast on 127.0.0.1:1 instead of ever touching
// the network; loopback httptest servers are exempt by default.
func TestMain(m *testing.M) {
	for _, kv := range [][2]string{
		{"HTTP_PROXY", "http://127.0.0.1:1"},
		{"HTTPS_PROXY", "http://127.0.0.1:1"},
		{"http_proxy", "http://127.0.0.1:1"},
		{"https_proxy", "http://127.0.0.1:1"},
	} {
		os.Setenv(kv[0], kv[1])
	}
	code := m.Run()
	if helperDir != "" {
		_ = os.RemoveAll(helperDir)
	}
	os.Exit(code)
}

// pinPlatform overrides the platform lookups for one test and restores them.
func pinPlatform(t *testing.T, goosVal, goarchVal string) {
	t.Helper()
	oldOS, oldArch := goosPinned, goarchPinned
	goosPinned, goarchPinned = goosVal, goarchVal
	t.Cleanup(func() { goosPinned, goarchPinned = oldOS, oldArch })
}

func TestPlatformLookupUnsupported(t *testing.T) {
	pinPlatform(t, "plan9", "mips")
	if _, err := platformChecksum(uvChecksums, "uv"); err == nil || !strings.Contains(err.Error(), "unsupported platform plan9-mips") {
		t.Fatalf("platformChecksum: err = %v", err)
	}
	if _, err := uvAssetNames(); err == nil || !strings.Contains(err.Error(), "unsupported platform plan9-mips for uv") {
		t.Fatalf("uvAssetNames: err = %v", err)
	}
	if _, err := pinchtabAssetName(); err == nil || !strings.Contains(err.Error(), "unsupported platform plan9-mips for pinchtab") {
		t.Fatalf("pinchtabAssetName: err = %v", err)
	}
}

func TestWindowsPathAndAssetNames(t *testing.T) {
	pinPlatform(t, "windows", "amd64")
	if name, err := uvAssetNames(); err != nil || name != "uv-x86_64-pc-windows-msvc.zip" {
		t.Fatalf("uvAssetNames = %q, %v", name, err)
	}
	if name, err := pinchtabAssetName(); err != nil || name != "pinchtab-windows-amd64.exe" {
		t.Fatalf("pinchtabAssetName = %q, %v", name, err)
	}
	if sum, err := platformChecksum(uvChecksums, "uv"); err != nil || sum != uvChecksums["windows-amd64"] {
		t.Fatalf("platformChecksum = %q, %v", sum, err)
	}
	// Windows path decorations on the runtime helpers.
	b := NewBrowserRuntime(filepath.Join(t.TempDir(), "pt"), zerolog.Nop())
	if want := filepath.Join(b.root, pinchtabVer, "pinchtab.exe"); b.binaryPath() != want {
		t.Fatalf("binaryPath = %q, want %q", b.binaryPath(), want)
	}
	py := NewPythonRuntime(filepath.Join(t.TempDir(), "rt"), t.TempDir(), zerolog.Nop())
	if got := py.uvBin(); !strings.HasSuffix(got, filepath.Join("bin", "uv.exe")) {
		t.Fatalf("uvBin = %q", got)
	}
	if got := py.venvPython("pdf"); !strings.HasSuffix(got, filepath.Join("venv", "Scripts", "python.exe")) {
		t.Fatalf("venvPython = %q", got)
	}
}

func TestFetchVerifiedFailurePaths(t *testing.T) {
	dir := t.TempDir()

	// Non-200 status.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()
	err := fetchVerified(t.Context(), srv.URL, strings.Repeat("a", 64), filepath.Join(dir, "a.bin"))
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("404: err = %v", err)
	}

	// Canceled context fails before any dial.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := fetchVerified(ctx, srv.URL, strings.Repeat("a", 64), filepath.Join(dir, "b.bin")); err == nil {
		t.Fatal("canceled ctx must fail")
	}

	// Destination parent is a regular file: MkdirAll fails.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fetchVerified(t.Context(), srv.URL, strings.Repeat("a", 64), filepath.Join(blocker, "c.bin")); err == nil {
		t.Fatal("dest under a file must fail")
	}

	// Unwritable directory: CreateTemp fails.
	ro := t.TempDir()
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	if err := fetchVerified(t.Context(), srv.URL, strings.Repeat("a", 64), filepath.Join(ro, "d.bin")); err == nil {
		t.Fatal("read-only dest dir must fail")
	}

	// Server truncates the body mid-download: the copy fails and the temp
	// file is removed.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4096")
		_, _ = w.Write([]byte("short"))
		panic(http.ErrAbortHandler)
	}))
	defer srv2.Close()
	if err := fetchVerified(t.Context(), srv2.URL, strings.Repeat("a", 64), filepath.Join(dir, "e.bin")); err == nil {
		t.Fatal("truncated body must fail")
	}
	leftover, _ := filepath.Glob(filepath.Join(dir, "e.bin.download-*"))
	if len(leftover) != 0 {
		t.Fatalf("failed download left temps: %v", leftover)
	}
}

// buildTarGz writes a .tar.gz whose members are prefixed with dir/ (uv wraps
// its binaries in a top-level directory).
func buildTarGz(t *testing.T, path, dir string, members map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// A directory entry and a duplicate-name member exercise the skips.
	_ = tw.WriteHeader(&tar.Header{Name: dir + "/", Typeflag: tar.TypeDir})
	_ = tw.WriteHeader(&tar.Header{Name: "elsewhere/skip.txt", Typeflag: tar.TypeReg, Size: int64(len("skip"))})
	_, _ = tw.Write([]byte("skip"))
	for name, content := range members {
		_ = tw.WriteHeader(&tar.Header{Name: dir + "/" + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))})
		_, _ = tw.Write([]byte(content))
		// Duplicate base name deeper in the tree must be ignored.
		_ = tw.WriteHeader(&tar.Header{Name: dir + "/sub/" + name, Typeflag: tar.TypeReg, Size: int64(len(content))})
		_, _ = tw.Write([]byte(content))
	}
	_ = tw.Close()
	_ = gz.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExtractArchiveMembersTarGz(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "uv-x86_64.tar.gz")
	buildTarGz(t, archive, "uv-x86_64", map[string]string{"uv": "#!/bin/sh\n: uv", "uvx": "#!/bin/sh\n: uvx"})

	dest := filepath.Join(dir, "out")
	if err := extractArchiveMembers(archive, dest, map[string]bool{"uv": true, "uvx": true}); err != nil {
		t.Fatalf("extract: %v", err)
	}
	for _, name := range []string{"uv", "uvx"} {
		raw, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || !strings.Contains(string(raw), name) {
			t.Fatalf("member %s not extracted: %v", name, err)
		}
		fi, err := os.Stat(filepath.Join(dest, name))
		if err != nil || fi.Mode().Perm() != 0o755 {
			t.Fatalf("member %s not executable: %v %v", name, fi, err)
		}
	}
}

func TestExtractArchiveMembersErrors(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.tar.gz")
	buildTarGz(t, good, "uv-x86_64", map[string]string{"uv": "uv"})
	want := map[string]bool{"uv": true}

	// Missing member (uvx is optional and must not matter).
	if err := extractArchiveMembers(good, filepath.Join(dir, "o1"), want); err != nil {
		t.Fatalf("uvx optional: %v", err)
	}
	if err := extractArchiveMembers(good, filepath.Join(dir, "o2"), map[string]bool{"nope": true}); err == nil ||
		!strings.Contains(err.Error(), `did not contain "nope"`) {
		t.Fatalf("missing member: %v", err)
	}
	// Destination already a directory: writeMember fails.
	out := filepath.Join(dir, "o3")
	if err := os.MkdirAll(filepath.Join(out, "uv"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := extractArchiveMembers(good, out, want); err == nil {
		t.Fatal("dest member is a dir: must fail")
	}
	// Missing archive file.
	if err := extractArchiveMembers(filepath.Join(dir, "absent.tar.gz"), filepath.Join(dir, "o4"), want); err == nil {
		t.Fatal("missing archive must fail")
	}
	// Not gzip.
	plain := filepath.Join(dir, "plain.tar.gz")
	if err := os.WriteFile(plain, []byte("not gzip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := extractArchiveMembers(plain, filepath.Join(dir, "o5"), want); err == nil ||
		!strings.Contains(err.Error(), "gunzip") {
		t.Fatalf("non-gzip: %v", err)
	}
	// Valid gzip wrapping tar garbage: tar read error.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte("garbage that is not a tar stream at all"))
	_ = gz.Close()
	junk := filepath.Join(dir, "junk.tar.gz")
	if err := os.WriteFile(junk, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := extractArchiveMembers(junk, filepath.Join(dir, "o6"), want); err == nil ||
		!strings.Contains(err.Error(), "read tar") {
		t.Fatalf("junk tar: %v", err)
	}
}

func buildZip(t *testing.T, path string, members map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range members {
		w, err := zw.Create("wrapped/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExtractZipMembers(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "uv.zip")
	buildZip(t, archive, map[string]string{"uv": "zip-uv", "uvx": "zip-uvx"})

	dest := filepath.Join(dir, "out")
	if err := extractArchiveMembers(archive, dest, map[string]bool{"uv": true, "uvx": true}); err != nil {
		t.Fatalf("extract zip: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dest, "uv"))
	if err != nil || string(raw) != "zip-uv" {
		t.Fatalf("zip member uv = %q, %v", raw, err)
	}
	// Missing member fails; a zip name routes through the same checkFound.
	if err := extractArchiveMembers(archive, filepath.Join(dir, "o2"), map[string]bool{"absent": true}); err == nil {
		t.Fatal("missing zip member must fail")
	}
	// Not a zip.
	plain := filepath.Join(dir, "plain.zip")
	if err := os.WriteFile(plain, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := extractArchiveMembers(plain, filepath.Join(dir, "o3"), map[string]bool{"uv": true}); err == nil ||
		!strings.Contains(err.Error(), "open zip") {
		t.Fatalf("non-zip: %v", err)
	}
}

func TestCheckFound(t *testing.T) {
	if err := checkFound(map[string]bool{"uvx": true}, map[string]bool{"uv": true, "uvx": true}); err == nil ||
		!strings.Contains(err.Error(), `did not contain "uv"`) {
		t.Fatalf("missing uv: %v", err)
	}
	if err := checkFound(map[string]bool{"uv": true}, map[string]bool{"uv": true, "uvx": true}); err != nil {
		t.Fatalf("uvx optional: %v", err)
	}
}

func TestPickPortRangeExhausted(t *testing.T) {
	// Occupy pinchtab's whole preferred range so the ephemeral fallback runs.
	var listeners []net.Listener
	defer func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
	}()
	for port := 9867; port <= 9877; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue // another service holds it: the range is partially blocked, fine
		}
		listeners = append(listeners, ln)
	}
	got := pickPort()
	if got == "" {
		t.Fatal("pickPort returned empty")
	}
	if n, err := strconv.Atoi(got); err != nil || n <= 0 {
		t.Fatalf("pickPort = %q: not a positive port (%v)", got, err)
	}
	// Only demand the fallback when we managed to hold the entire range.
	if len(listeners) == 11 {
		if n, _ := strconv.Atoi(got); n >= 9867 && n <= 9877 {
			t.Fatalf("pickPort = %s despite the whole range being held", got)
		}
	}
}

func TestChecksumTablesWellFormed(t *testing.T) {
	for key, sum := range uvChecksums {
		if _, err := hex.DecodeString(sum); err != nil {
			t.Fatalf("uv checksum for %s not hex: %v", key, err)
		}
	}
	for key, sum := range pinchtabChecksums {
		if _, err := hex.DecodeString(sum); err != nil {
			t.Fatalf("pinchtab checksum for %s not hex: %v", key, err)
		}
	}
}

func TestFetchVerifiedBodyReset(t *testing.T) {
	// A connection reset mid-body fails the streaming copy and leaves no
	// temp files behind.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resetConn(w)
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := fetchVerified(t.Context(), srv.URL, strings.Repeat("a", 64), filepath.Join(dir, "r.bin")); err == nil {
		t.Fatal("reset mid-download must fail")
	}
	leftover, _ := filepath.Glob(filepath.Join(dir, "r.bin.download-*"))
	if len(leftover) != 0 {
		t.Fatalf("failed download left temps: %v", leftover)
	}
}

func TestExtractZipSkipsDirEntries(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if _, err := zw.CreateHeader(&zip.FileHeader{Name: "wrapped/", Method: zip.Store}); err != nil {
		t.Fatal(err)
	}
	w, err := zw.Create("wrapped/uv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("dir-entry-uv")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "u.zip")
	if err := os.WriteFile(archive, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "out")
	if err := extractArchiveMembers(archive, dest, map[string]bool{"uv": true}); err != nil {
		t.Fatalf("zip with dir entry: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dest, "uv"))
	if err != nil || string(raw) != "dir-entry-uv" {
		t.Fatalf("uv = %q, %v", raw, err)
	}
}

func TestExtractArchiveDestUnwritable(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "g.tar.gz")
	buildTarGz(t, archive, "uv-x86_64", map[string]string{"uv": "uv"})
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// destDir below a regular file: writeMember's MkdirAll fails.
	if err := extractArchiveMembers(archive, filepath.Join(blocker, "sub"), map[string]bool{"uv": true}); err == nil {
		t.Fatal("dest under a file must fail")
	}
}

func TestBuiltinDirAndPythonBuiltins(t *testing.T) {
	browser := mustBuiltin(t, "browser")
	if browser.Dir() != "golang/browser" {
		t.Fatalf("browser Dir = %q", browser.Dir())
	}
	pys := PythonBuiltins()
	if len(pys) != 2 || pys[0].Name != "office" || pys[1].Name != "pdf" {
		t.Fatalf("PythonBuiltins = %+v", pys)
	}
	for _, py := range pys {
		if py.Kind != "python" {
			t.Fatalf("python builtin %s kind = %s", py.Name, py.Kind)
		}
	}
	if got := mustBuiltin(t, "pdf").Dir(); got != "python/pdf" {
		t.Fatalf("pdf Dir = %q", got)
	}
	if got := mustBuiltin(t, "office").Dir(); got != "python/office" {
		t.Fatalf("office Dir = %q", got)
	}
	if _, ok := ByName("does-not-exist"); ok {
		t.Fatal("unknown name must not resolve")
	}
}

func TestFetchVerifiedRequestAndTempFailures(t *testing.T) {
	dir := t.TempDir()
	// A control character in the URL fails request construction.
	if err := fetchVerified(t.Context(), "http://bad\x7fhost/x", strings.Repeat("a", 64), filepath.Join(dir, "u.bin")); err == nil {
		t.Fatal("invalid URL must fail")
	}
	// A readable 200 server plus an unwritable dest dir: CreateTemp fails
	// after a successful response.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()
	ro := t.TempDir()
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	if err := fetchVerified(t.Context(), srv.URL, strings.Repeat("a", 64), filepath.Join(ro, "v.bin")); err == nil {
		t.Fatal("read-only dest dir must fail at temp creation")
	}
}

func TestExtractZipMemberWriteFails(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "u.zip")
	buildZip(t, archive, map[string]string{"uv": "uv"})
	dest := filepath.Join(dir, "out")
	if err := os.MkdirAll(filepath.Join(dest, "uv"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := extractArchiveMembers(archive, dest, map[string]bool{"uv": true}); err == nil {
		t.Fatal("zip member dest blocked by a dir must fail")
	}
}

// pinChecksum overrides the pinned digest for the current platform (tests
// serve locally generated artifacts) and restores it afterwards.
func pinChecksum(t *testing.T, table map[string]string, sum string) {
	t.Helper()
	key := goos() + "-" + goarch()
	old, had := table[key]
	table[key] = sum
	t.Cleanup(func() {
		if had {
			table[key] = old
			return
		}
		delete(table, key)
	})
}

func TestBrowserInstallFromLocalRelease(t *testing.T) {
	bin := []byte("#!/bin/sh\necho local-pinchtab\n")
	sum := sha256.Sum256(bin)
	pinChecksum(t, pinchtabChecksums, hex.EncodeToString(sum[:]))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, pinchtabAssetNameOf(t)) {
			http.Error(w, "wrong asset", http.StatusNotFound)
			return
		}
		_, _ = w.Write(bin)
	}))
	defer srv.Close()
	oldBase := ptReleaseBase
	ptReleaseBase = srv.URL
	t.Cleanup(func() { ptReleaseBase = oldBase })

	b := NewBrowserRuntime(t.TempDir(), zerolog.Nop())
	var lines []string
	if err := b.Install(t.Context(), func(line string) { lines = append(lines, line) }); err != nil {
		t.Fatalf("install from local release: %v", err)
	}
	raw, err := os.ReadFile(b.binaryPath())
	if err != nil || string(raw) != string(bin) {
		t.Fatalf("installed binary = %q, %v", raw, err)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "verifying sha256… ok") {
		t.Fatalf("progress = %v", lines)
	}
	// Second install is a no-op.
	if err := b.Install(t.Context(), nil); err != nil {
		t.Fatalf("idempotent reinstall: %v", err)
	}
}

func pinchtabAssetNameOf(t *testing.T) string {
	t.Helper()
	name, err := pinchtabAssetName()
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func TestPythonInstallDownloadsUV(t *testing.T) {
	// Serve a release archive whose uv member is the fake uv script: the
	// runtime downloads it, verifies the digest, extracts it and then runs
	// it for the venv steps.
	runtimes, home := t.TempDir(), t.TempDir()
	py := NewPythonRuntime(runtimes, home, zerolog.Nop())

	asset, err := uvAssetNames()
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), asset)
	buildTarGz(t, archive, "uv-x86_64", map[string]string{"uv": fakeUvScript, "uvx": fakeUvScript})
	rawArchive, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(rawArchive)
	pinChecksum(t, uvChecksums, hex.EncodeToString(sum[:]))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, asset) {
			http.Error(w, "wrong asset", http.StatusNotFound)
			return
		}
		_, _ = w.Write(rawArchive)
	}))
	defer srv.Close()
	oldBase := uvReleaseBase
	uvReleaseBase = srv.URL
	t.Cleanup(func() { uvReleaseBase = oldBase })

	var lines []string
	if err := py.Install(t.Context(), mustBuiltin(t, "pdf"), func(line string) {
		lines = append(lines, line)
	}); err != nil {
		t.Fatalf("install with downloaded uv: %v", err)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "uv installed (shared runtimes)") || !strings.Contains(joined, "verifying sha256… ok") {
		t.Fatalf("progress = %v", lines)
	}
	if !py.Ready("pdf") {
		t.Fatal("env not ready after downloaded-uv install")
	}
	if _, err := os.Stat(py.uvBin()); err != nil {
		t.Fatalf("uv not extracted: %v", err)
	}
	// The downloaded archive itself is not kept under runtimes/bin.
	if _, err := os.Stat(filepath.Join(runtimes, "bin", asset)); err == nil {
		t.Fatal("release archive must be removed after extraction")
	}
}

func TestPythonInstallArchiveMissingUV(t *testing.T) {
	// A digest-valid archive without the uv member: the download succeeds
	// and the extraction step rejects it.
	py := NewPythonRuntime(t.TempDir(), t.TempDir(), zerolog.Nop())
	asset, err := uvAssetNames()
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), asset)
	buildTarGz(t, archive, "uv-x86_64", map[string]string{"uvx": "not uv"})
	rawArchive, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(rawArchive)
	pinChecksum(t, uvChecksums, hex.EncodeToString(sum[:]))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(rawArchive)
	}))
	defer srv.Close()
	oldBase := uvReleaseBase
	uvReleaseBase = srv.URL
	t.Cleanup(func() { uvReleaseBase = oldBase })

	err = py.Install(t.Context(), mustBuiltin(t, "pdf"), nil)
	if err == nil || !strings.Contains(err.Error(), "extract uv") {
		t.Fatalf("err = %v, want extract uv failure", err)
	}
}
