// Package selfupdate implements ARTEX's one-click update from the UI: it fetches the new binary
// from a GitHub Release, verifies it, stages it, and swaps it in atomically on the next start.
//
// Division of labour (see start.sh / start.bat):
//
//	the start script = a dumb supervisor loop whose only job is "after the process exits, decide from the exit code whether to restart it"
//	this package     = all the error-prone logic (download / SHA256 verification / smoke test / swap / rollback on failure)
//
// The swap lives in Go rather than in the script because the sha256 check and the smoke test would
// have to be written twice for sh and bat (sha256sum / shasum / certutil), and that is exactly the
// part that must never go wrong -- swap in a binary that cannot start and the supervisor will
// faithfully restart it over and over, leaving the user to rescue the machine by hand.
//
// A complete upgrade spans three process starts:
//
//	(1) the old server receives /api/update/apply -> download and verify -> stage artex.new -> exit 75
//	(2) the script restarts the old version -> Bootstrap finds artex.new -> verify + smoke test -> swap -> exit 75
//	(3) the script restarts, now on the new version -> Bootstrap records an attempt -> the marker is cleared once the start succeeds
//
// A failure at any step falls back to the old version: if (2) does not verify, the staged file is
// deleted and the old version keeps running; if (3) fails to survive long enough to clear the marker
// 3 times in a row (i.e. it crashes on start), artex.old is swapped back automatically.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExitRestart is the exit code meaning "supervisor, please restart me" (EX_TEMPFAIL). The start
// script sees it and reruns immediately without counting it towards the crash backoff. 0 means the
// user stopped normally (the script leaves the loop); anything else is treated as a crash.
const ExitRestart = 75

// maxAttempts is how many start attempts are allowed after a swap. Each start of the new version
// increments the counter and clears the marker once it survives settleDelay; crashing maxAttempts
// times in a row means the new version simply cannot start, so it is rolled back automatically.
const maxAttempts = 3

// Paths holds every file involved in an upgrade, all kept **in the directory containing the executable**.
// Deliberately not the CWD: when running as a service the working directory may be / or anything
// else, which would put the staged file somewhere unexpected and break the swap logic entirely.
type Paths struct {
	Dir     string // directory containing the executable
	Current string // the binary currently running        artex      / artex.exe
	New     string // the staged new version              artex.new  / artex.new.exe
	Sum     string // the new version's sha256 (hex)      artex.new.sha256 / artex.new.exe.sha256
	Old     string // the old version backed up before the swap   artex.old  / artex.old.exe
	Marker  string // the upgrade state marker            artex.upgrade.json
}

// ResolvePaths derives every upgrade path from the current executable.
//
// On Windows the .new/.old files must carry the .exe suffix too, or both the smoke test and running
// the binary after the swap would fail, so the suffix is stripped first and then re-appended; that
// keeps the naming symmetric across both platforms.
func ResolvePaths() (Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return Paths{}, fmt.Errorf("locate the executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	name := filepath.Base(exe)
	ext := filepath.Ext(name) // ".exe" on Windows, usually empty on Unix
	stem := strings.TrimSuffix(name, ext)

	join := func(suffix string) string { return filepath.Join(dir, stem+suffix+ext) }
	return Paths{
		Dir:     dir,
		Current: exe,
		New:     join(".new"),
		Sum:     join(".new") + ".sha256",
		Old:     join(".old"),
		Marker:  filepath.Join(dir, stem+".upgrade.json"),
	}, nil
}

// marker records the progress of one swap, used to trigger an automatic rollback when the new version will not start.
type marker struct {
	From     string `json:"from"`     // the version before the upgrade
	To       string `json:"to"`       // the target version
	Attempts int    `json:"attempts"` // how many start attempts have been made since the swap
	StagedAt int64  `json:"staged_at"`
}

func readMarker(path string) (marker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return marker{}, false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil {
		return marker{}, false
	}
	return m, true
}

func writeMarker(path string, m marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// cleanStaged removes the staged file. A successful swap, a failed verification and a user
// cancellation all go through it, so a leftover artex.new is never retried on the next start.
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions compares two version numbers and returns -1/0/1 (a<b / a==b / a>b).
// ok=false means at least one side is not a comparable version (e.g. a local development build's
// "dev", or "0.3.7-2-gabc1234-dirty" produced by git describe); in that case the caller must disable
// one-click updates, or a development build would be "upgraded" to a release and uncommitted changes overwritten.
func CompareVersions(a, b string) (int, bool) {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range 3 {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseVersion parses a version of the form "v0.3.7" / "0.3.7" into a [3]int.
//
// Only a clean three-part version is accepted: on a non-tag build, build.sh uses git describe and
// produces suffixed versions like "0.3.7-2-gabc1234". Those must be judged incomparable rather than
// treated as 0.3.7 -- otherwise a development build would be mistaken for "already up to date" or overwritten by a release.
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return [3]int{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// InDocker reports whether the process is running in a container. Under Docker a swap writes to the
// container's writable layer, and recreating the container with `docker compose up -d` reverts to the
// version shipped in the image -- that is expected behaviour (the user is pulling a new image at that
// point), but the frontend needs to be able to say so clearly.
func InDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd")
}
