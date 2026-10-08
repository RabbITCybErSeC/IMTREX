package selfupdate

import (
	"archive/zip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testPaths builds an isolated upgrade directory. ResolvePaths() cannot be used directly -- it would
// point at the test binary itself and renaming go test's executable on the first run.
func testPaths(t *testing.T) Paths {
	t.Helper()
	dir := t.TempDir()
	return Paths{
		Dir:     dir,
		Current: filepath.Join(dir, "artex"),
		New:     filepath.Join(dir, "artex.new"),
		Sum:     filepath.Join(dir, "artex.new.sha256"),
		Old:     filepath.Join(dir, "artex.old"),
		Marker:  filepath.Join(dir, "artex.upgrade.json"),
	}
}

// fakeBin writes an executable shell script standing in for artex. smokeTest only runs it with -h
// and checks the exit code, so a script is entirely sufficient and far faster than building a real binary.
func fakeBin(t *testing.T, path, marker string, exitCode int) {
	t.Helper()
	script := "#!/bin/sh\necho " + marker + "\nexit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake binary %s: %v", path, err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return string(rune('0' + n))
}

// stage arranges bin to look "staged and awaiting a swap": it writes artex.new and its checksum.
func stage(t *testing.T, p Paths, marker string, exitCode int) {
	t.Helper()
	fakeBin(t, p.New, marker, exitCode)
	sum, err := fileSHA256(p.New)
	if err != nil {
		t.Fatalf("compute checksum: %v", err)
	}
	if err := os.WriteFile(p.Sum, []byte(sum), 0o644); err != nil {
		t.Fatalf("write checksum: %v", err)
	}
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func requireUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is an sh script and cannot run on Windows")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b       string
		want       int
		comparable bool
	}{
		{"0.3.7", "0.3.8", -1, true},
		{"0.3.8", "0.3.7", 1, true},
		{"0.3.7", "0.3.7", 0, true},
		{"v0.3.7", "0.3.8", -1, true}, // build.sh strips the v, tags carry it; both forms must be accepted
		{"0.3.7", "v0.3.7", 0, true},
		{"0.9.0", "0.10.0", -1, true}, // compared numerically, not lexicographically
		{"1.0.0", "0.99.99", 1, true},
		// Development builds must be judged incomparable, or a release would overwrite uncommitted changes.
		{"dev", "0.3.8", 0, false},
		{"0.3.7-2-gabc1234", "0.3.8", 0, false},
		{"0.3.7-dirty", "0.3.8", 0, false},
		{"0.3", "0.3.8", 0, false},
		{"", "0.3.8", 0, false},
	}
	for _, c := range cases {
		got, ok := CompareVersions(c.a, c.b)
		if ok != c.comparable {
			t.Errorf("CompareVersions(%q,%q) comparable=%v, want %v", c.a, c.b, ok, c.comparable)
			continue
		}
		if ok && got != c.want {
			t.Errorf("CompareVersions(%q,%q)=%d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestResolvePathsNaming(t *testing.T) {
	p, err := ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	// Key invariant: every upgrade file sits next to the executable. Falling back to the CWD would
	// completely break the swap when running as a service (where the working directory may be /).
	for name, path := range map[string]string{"New": p.New, "Sum": p.Sum, "Old": p.Old, "Marker": p.Marker} {
		if filepath.Dir(path) != p.Dir {
			t.Errorf("%s is not in the executable's directory: %s (want %s)", name, path, p.Dir)
		}
	}
	// On Windows, .new/.old must keep the .exe suffix, or both the smoke test and running the binary after the swap fail.
	if runtime.GOOS == "windows" {
		if !strings.HasSuffix(p.New, ".exe") || !strings.HasSuffix(p.Old, ".exe") {
			t.Errorf("on Windows .new/.old must end in .exe: new=%s old=%s", p.New, p.Old)
		}
	}
}

func TestVerifyStagedRejectsTamperedBinary(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	stage(t, p, "new", 0)

	// Modify the file after the checksum is written, simulating a corrupted or swapped download.
	fakeBin(t, p.New, "tampered", 0)
	if err := verifyStaged(p); err == nil {
		t.Fatal("expected a SHA256 mismatch to be rejected, but it passed")
	}
}

func TestVerifyStagedRejectsUnrunnableBinary(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	stage(t, p, "broken", 1) // executes, but with a non-zero exit code

	if err := verifyStaged(p); err == nil {
		t.Fatal("expected a failed smoke test to be rejected, but it passed")
	}
}

func TestApplyStagedHappyPath(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8"}); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	action, st := applyStaged(p)
	if action != Restart {
		t.Fatalf("want Restart, got %v", action)
	}
	if !st.Pending {
		t.Error("the state should be Pending after a swap")
	}
	if !strings.Contains(readAll(t, p.Current), "new") {
		t.Error("artex should have been replaced by the new version")
	}
	if !strings.Contains(readAll(t, p.Old), "old") {
		t.Error("the old version should be backed up to artex.old")
	}
	if _, err := os.Stat(p.New); !os.IsNotExist(err) {
		t.Error("artex.new should be gone after the swap")
	}
	if _, err := os.Stat(p.Sum); !os.IsNotExist(err) {
		t.Error("the checksum file should be cleaned up after the swap")
	}
	// The marker must stay: the next start (running the new version) uses it to count attempts and roll back if needed.
	if _, ok := readMarker(p.Marker); !ok {
		t.Error("the upgrade marker should be kept after the swap")
	}
}

func TestApplyStagedKeepsCurrentWhenVerifyFails(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	fakeBin(t, p.New, "tampered", 0) // break the checksum

	action, st := applyStaged(p)
	if action != Continue {
		t.Fatalf("want Continue when verification fails, got %v", action)
	}
	if !st.FailedStage {
		t.Error("the state should be flagged FailedStage")
	}
	if !strings.Contains(readAll(t, p.Current), "old") {
		t.Fatal("the current version must never be touched when verification fails")
	}
	if _, err := os.Stat(p.New); !os.IsNotExist(err) {
		t.Error("a staged file that failed verification should be cleaned up, or the next start retries it")
	}
}

func TestSwapOverwritesPreviousBackup(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeBin(t, p.Old, "v1", 0) // a backup left by a previous upgrade
	stage(t, p, "v3", 0)

	if err := swap(p); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if !strings.Contains(readAll(t, p.Current), "v3") {
		t.Error("should have swapped in v3")
	}
	if !strings.Contains(readAll(t, p.Old), "v2") {
		t.Error("the backup should be updated to the v2 that was just swapped out")
	}
}

func TestConfirmCountsAttemptsThenRollsBack(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "broken-new", 0)
	fakeBin(t, p.Old, "good-old", 0)
	m := marker{From: "0.3.7", To: "0.3.8"}

	// The first maxAttempts starts only increment the counter, giving the new version a chance to settle.
	for i := 1; i <= maxAttempts; i++ {
		action, st := confirmOrRollback(p, m)
		if action != Continue {
			t.Fatalf("attempt %d: want Continue, got %v", i, action)
		}
		if !st.Pending {
			t.Errorf("attempt %d: the state should be Pending", i)
		}
		got, ok := readMarker(p.Marker)
		if !ok || got.Attempts != i {
			t.Fatalf("after attempt %d attempts=%d (ok=%v), want %d", i, got.Attempts, ok, i)
		}
		m = got
	}

	// One more crash exceeds the limit and automatically puts the old version back.
	action, st := confirmOrRollback(p, m)
	if action != Restart {
		t.Fatalf("want Restart once the attempt limit is exceeded, got %v", action)
	}
	if !st.RolledBack {
		t.Error("the state should be flagged RolledBack")
	}
	if !strings.Contains(readAll(t, p.Current), "good-old") {
		t.Fatal("should have rolled back to the old version")
	}
	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Error("the marker should be cleared after a rollback, or it would roll back forever")
	}
	// Keep the version that would not start for diagnosis instead of deleting it.
	if _, err := os.Stat(p.Current + ".failed"); err != nil {
		t.Error("the failed version should be kept as .failed for diagnosis")
	}
}

func TestManualRollbackIsReversible(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeBin(t, p.Old, "v1", 0)

	// Rollback() goes through ResolvePaths(); this tests the underlying swap semantics directly.
	tmp := p.Current + ".swap"
	if err := os.Rename(p.Current, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readAll(t, p.Current), "v1") {
		t.Error("the current version should be v1 after the rollback")
	}
	if !strings.Contains(readAll(t, p.Old), "v2") {
		t.Error("the backup should become v2 after the rollback, so it can be rolled forward again")
	}
}

func TestParseSums(t *testing.T) {
	const (
		linuxSum = "1111111111111111111111111111111111111111111111111111111111111111"
		winSum   = "ABCDEF0000000000000000000000000000000000000000000000000000000000"
	)
	// sha256sum output is separated by two spaces; shasum -a 256 prefixes the filename with * in binary mode.
	raw := linuxSum + "  artex-0.3.8-linux-amd64.zip\n" +
		winSum + " *artex-0.3.8-windows-amd64.zip\n" +
		"\n" +
		"garbage line\n" + // exactly two fields, but the first is not a digest
		"deadbeef  artex-0.3.8-darwin-arm64.zip\n" // the digest has the wrong length

	out := parseSums(raw)
	if out["artex-0.3.8-linux-amd64.zip"] != linuxSum {
		t.Errorf("the linux entry parsed incorrectly: %v", out)
	}
	// Digests are normalized to lower case so comparison never fails on case alone.
	if got := out["artex-0.3.8-windows-amd64.zip"]; got != strings.ToLower(winSum) {
		t.Errorf("windows entry is wrong (the * prefix should be stripped and the digest lower-cased): %q", got)
	}
	if len(out) != 2 {
		t.Errorf("blank lines, non-digest lines and wrong-length lines should be ignored, got %v", out)
	}
}

func TestExtractBinaryFindsNestedEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the base name inside the package is artex.exe on Windows; this case is built around Unix naming")
	}
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")

	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	// The structure of a real release package: artex-<version>-<os>-<arch>/artex, plus some decoy files.
	for name, body := range map[string]string{
		"artex-0.3.8-linux-amd64/README.md":           "readme",
		"artex-0.3.8-linux-amd64/skills/a.md":         "skill",
		"artex-0.3.8-linux-amd64/artex":               "#!/bin/sh\nexit 0\n",
		"artex-0.3.8-linux-amd64/config.example.json": "{}",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	dst := filepath.Join(dir, "out")
	if err := extractBinary(zipPath, dst); err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if got := readAll(t, dst); !strings.Contains(got, "exit 0") {
		t.Errorf("what was extracted is not the artex executable: %q", got)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("the extracted binary must carry the execute bit")
	}
}

func TestExtractBinaryMissingEntry(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("artex-0.3.8-linux-amd64/README.md")
	_, _ = w.Write([]byte("readme"))
	_ = zw.Close()
	f.Close()

	if err := extractBinary(zipPath, filepath.Join(dir, "out")); err == nil {
		t.Fatal("an error is expected when the package contains no executable")
	}
}

func TestCheckURLRejectsNonGitHub(t *testing.T) {
	bad := []string{
		"http://github.com/x",           // not HTTPS
		"https://evil.com/artex.zip",    // domain not on the allowlist
		"https://github.com.evil.com/x", // suffix spoofing
		"https://raw.githubusercontent.com.evil.com/x",
	}
	for _, raw := range bad {
		u := mustParse(t, raw)
		if err := checkURL(u); err == nil {
			t.Errorf("checkURL(%q) should be rejected", raw)
		}
	}
	good := []string{
		"https://api.github.com/repos/x/releases/latest",
		"https://objects.githubusercontent.com/blah",
		"https://GitHub.com/x", // the domain is case-insensitive
	}
	for _, raw := range good {
		u := mustParse(t, raw)
		if err := checkURL(u); err != nil {
			t.Errorf("checkURL(%q) should be allowed, but failed: %v", raw, err)
		}
	}
}

func TestAssetNameMatchesBuildScript(t *testing.T) {
	// build.sh's package_binary uses artex-<version>-<os>-<arch>.zip, with the v prefix stripped from
	// the version. One wrong character here and one-click update finds no asset on any platform.
	if got := AssetName("v0.3.8", "linux", "amd64"); got != "artex-0.3.8-linux-amd64.zip" {
		t.Errorf("AssetName = %q", got)
	}
	if got := AssetName("0.3.8", "windows", "amd64"); got != "artex-0.3.8-windows-amd64.zip" {
		t.Errorf("AssetName = %q", got)
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

func TestSettleClearsMarkerAndStopsRollback(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "new", 0)
	fakeBin(t, p.Old, "old", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8", Attempts: 2}); err != nil {
		t.Fatal(err)
	}

	settle(p)

	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Fatal("the upgrade marker must be cleared once stability is confirmed")
	}
	// With the marker gone, later normal restarts no longer count attempts or wrongly trigger a rollback.
	if _, ok := readMarker(p.Marker); ok {
		t.Error("reading the marker should fail")
	}
	// Keep the backup so the user can still roll back by hand.
	if _, err := os.Stat(p.Old); err != nil {
		t.Error("the previous version's backup should still be kept after stability is confirmed")
	}
}

func TestSettleIsNoopWithoutMarker(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "cur", 0)
	settle(p) // the normal start path: must not panic and must not touch any file
	if _, err := os.Stat(p.Current); err != nil {
		t.Error("settle must not affect any file when there is no marker")
	}
}
