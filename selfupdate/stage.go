package selfupdate

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"runtime"
	"strings"
	"time"
)

// sumsAsset is the checksum manifest produced by release.yml; it covers every zip in the Release.
const sumsAsset = "SHA256SUMS"

// maxBinarySize caps the size of the extracted binary so a malformed zip cannot fill the disk.
const maxBinarySize = 512 << 20 // 512 MiB

// Phase is a stage of the upgrade process, used directly as the phase field in SSE events.
type Phase string

const (
	PhaseIdle     Phase = "idle"
	PhaseDownload Phase = "downloading"
	PhaseVerify   Phase = "verifying"
	PhaseExtract  Phase = "extracting"
	PhaseStaged   Phase = "staged"
	PhaseFailed   Phase = "failed"
)

// Progress is supplied by the caller to push progress to the frontend. pct is only meaningful
// during the download phase (0-100); other phases pass -1.
type Progress func(ph Phase, pct int, msg string)

// Stage downloads the release package for the current platform from the given Release, verifies it and stages the new binary as artex.new.
//
// It goes through the full zip rather than a bare binary for two reasons: the existing Releases'
// SHA256SUMS only cover the zips, so using the zip needs no CI change and stays compatible with
// already published versions; and the zip also carries skills/, leaving the door open for syncing
// builtin skills later. The only cost is downloading the few hundred KB of skills.
//
// Returning from this function means staging is complete; the caller then shuts down gracefully and exits with ExitRestart.
func Stage(ctx context.Context, c *http.Client, rel *Release, currentVersion string, prog Progress) error {
	if prog == nil {
		prog = func(Phase, int, string) {}
	}
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if err := checkWritable(p.Dir); err != nil {
		return err
	}

	name := AssetName(rel.TagName, runtime.GOOS, runtime.GOARCH)
	asset, ok := rel.FindAsset(name)
	if !ok {
		return fmt.Errorf("this version ships no release package for %s/%s (%s is missing)", runtime.GOOS, runtime.GOARCH, name)
	}

	prog(PhaseDownload, 0, "Fetching the checksum manifest...")
	sums, err := fetchSums(ctx, c, rel)
	if err != nil {
		return err
	}
	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("%s does not list %s; refusing to install an unverified binary", sumsAsset, name)
	}

	// All temporary files live in the target directory so the final rename is an atomic operation
	// within one filesystem (a cross-device rename fails, and /tmp is often a separate mount point).
	zipPath := p.New + ".zip.part"
	binPath := p.New + ".part"
	defer func() {
		_ = os.Remove(zipPath)
		_ = os.Remove(binPath)
	}()

	prog(PhaseDownload, 0, fmt.Sprintf("Downloading %s (%s)...", name, humanSize(asset.Size)))
	got, err := download(ctx, c, asset, zipPath, prog)
	if err != nil {
		return err
	}

	prog(PhaseVerify, -1, "Verifying SHA256...")
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("SHA256 mismatch: expected %s, got %s (corrupted download or tampering)", short(want), short(got))
	}

	prog(PhaseExtract, -1, "Extracting and smoke testing...")
	if err := extractBinary(zipPath, binPath); err != nil {
		return err
	}
	if err := smokeTest(binPath); err != nil {
		return fmt.Errorf("the new version does not run on this system: %w", err)
	}

	// The staged file's own sha256 is stored separately: it is verified once more before the swap on
	// the next start, guarding against the file being modified or corrupted between staging and restart.
	binSum, err := fileSHA256(binPath)
	if err != nil {
		return fmt.Errorf("compute the new binary's checksum: %w", err)
	}
	if err := os.WriteFile(p.Sum, []byte(binSum), 0o644); err != nil {
		return fmt.Errorf("write checksum: %w", err)
	}
	if err := os.Rename(binPath, p.New); err != nil {
		_ = os.Remove(p.Sum)
		return fmt.Errorf("stage the new version: %w", err)
	}

	if err := writeMarker(p.Marker, marker{
		From:     currentVersion,
		To:       strings.TrimPrefix(rel.TagName, "v"),
		StagedAt: time.Now().Unix(),
	}); err != nil {
		// The marker only affects automatic rollback; the staged file itself is in place, so this does not abort the upgrade.
		prog(PhaseStaged, -1, "Warning: failed to write the upgrade marker; this upgrade has no automatic rollback protection")
	}

	prog(PhaseStaged, 100, "The new version is ready, restarting...")
	return nil
}

// fetchSums downloads and parses SHA256SUMS, returning filename -> hex digest.
func fetchSums(ctx context.Context, c *http.Client, rel *Release) (map[string]string, error) {
	asset, ok := rel.FindAsset(sumsAsset)
	if !ok {
		return nil, fmt.Errorf("this Release has no %s, so integrity cannot be verified; refusing to upgrade", sumsAsset)
	}
	body, err := get(ctx, c, asset.URL)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", sumsAsset, err)
	}
	defer body.Close()

	raw, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", sumsAsset, err)
	}
	out := parseSums(string(raw))
	if len(out) == 0 {
		return nil, fmt.Errorf("%s is empty or in an unrecognized format", sumsAsset)
	}
	return out, nil
}

// parseSums parses a sha256sum-style manifest, returning filename -> hex digest.
//
// A line is only accepted when its first field is 64 hex characters. Checking for "exactly two
// fields" is not enough -- any two-word line of prose would be taken as a valid entry, stuffing
// junk into the digest table, and a real asset could then match the wrong digest.
func parseSums(raw string) map[string]string {
	out := map[string]string{}
	for line := range strings.Lines(raw) {
		// The format is "<sha256>  <filename>" (sha256sum uses two spaces; shasum's binary
		// mode prefixes the filename with *).
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 || !isHexSHA256(fields[0]) {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == "" {
			continue
		}
		out[name] = strings.ToLower(fields[0])
	}
	return out
}

func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// download writes the asset to dst while computing its SHA256 and reporting progress based on Content-Length.
func download(ctx context.Context, c *http.Client, a Asset, dst string, prog Progress) (string, error) {
	body, err := get(ctx, c, a.URL)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", a.Name, err)
	}
	defer body.Close()

	f, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("create temporary file: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	pw := &progressWriter{total: a.Size, prog: prog, name: a.Name, last: time.Now()}
	if _, err := io.Copy(io.MultiWriter(f, h, pw), body); err != nil {
		return "", fmt.Errorf("download interrupted: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("failed to write to disk: %w", err)
	}
	if a.Size > 0 && pw.written != a.Size {
		return "", fmt.Errorf("incomplete download: expected %d bytes, got %d", a.Size, pw.written)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// get issues a GET constrained by the allowlist and returns the response body.
func get(ctx context.Context, c *http.Client, rawURL string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "artex-selfupdate")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// extractBinary pulls the artex executable out of the release package.
//
// The layout inside the package is artex-<version>-<os>-<arch>/artex, but matching is done on the
// **base name** rather than assembling the full path: the version appears in the package name, one
// wrong character would fail the whole upgrade, and matching by base name is more robust.
func extractBinary(zipPath, dst string) error {
	want := "artex"
	if runtime.GOOS == "windows" {
		want = "artex.exe"
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open the release package: %w", err)
	}
	defer zr.Close()

	for _, entry := range zr.File {
		if entry.FileInfo().IsDir() || !strings.EqualFold(path.Base(entry.Name), want) {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			return fmt.Errorf("read %s: %w", entry.Name, err)
		}
		defer rc.Close()

		f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return fmt.Errorf("write out the new binary: %w", err)
		}
		defer f.Close()

		n, err := io.Copy(f, io.LimitReader(rc, maxBinarySize+1))
		if err != nil {
			return fmt.Errorf("extract %s: %w", entry.Name, err)
		}
		if n > maxBinarySize {
			return fmt.Errorf("the executable inside the release package exceeds %s, refusing to extract", humanSize(maxBinarySize))
		}
		if n == 0 {
			return fmt.Errorf("%s inside the release package is empty", want)
		}
		return f.Sync()
	}
	return fmt.Errorf("%s was not found in the release package", want)
}

// checkWritable confirms up front that the directory is writable. Without this step, running as a
// non-root user or keeping the binary in a system directory would only fail at swap time, after tens of MB have been downloaded.
func checkWritable(dir string) error {
	probe, err := os.CreateTemp(dir, ".artex-update-probe-*")
	if err != nil {
		return fmt.Errorf("the program directory %s is not writable, automatic update is impossible (check the permissions or upgrade manually): %w", dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// progressWriter counts the bytes written and rate-limits reporting so not every 32KiB chunk pushes an SSE event.
type progressWriter struct {
	total   int64
	written int64
	name    string
	prog    Progress
	last    time.Time
}

func (w *progressWriter) Write(b []byte) (int, error) {
	w.written += int64(len(b))
	if time.Since(w.last) < 300*time.Millisecond {
		return len(b), nil
	}
	w.last = time.Now()
	pct := -1
	if w.total > 0 {
		pct = int(w.written * 100 / w.total)
	}
	w.prog(PhaseDownload, pct, fmt.Sprintf("Downloading %s / %s", humanSize(w.written), humanSize(w.total)))
	return len(b), nil
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12] + "..."
	}
	return sum
}
