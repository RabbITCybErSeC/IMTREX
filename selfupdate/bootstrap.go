package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// smokeEnv makes the child process started by the smoke test skip Bootstrap outright.
//
// Strictly speaking nothing would break without it: the child's os.Executable() is artex.new, so
// every path derived from it carries the .new prefix and never touches the real upgrade files. But
// relying on that coincidence is fragile; short-circuiting explicitly is obvious at a glance and
// saves the child a pointless disk probe.
const smokeEnv = "ARTEX_SELFUPDATE_SMOKE"

// Action is the instruction Bootstrap gives main.
type Action int

const (
	// Continue: start the server as usual.
	Continue Action = iota
	// Restart: exit immediately with ExitRestart so the supervisor script restarts us.
	Restart
)

// State describes the upgrade status at this start, so /api/update/check can tell the frontend
// truthfully "whether the last upgrade succeeded or was rolled back".
type State struct {
	Pending     bool   // swapped in but not yet confirmed stable
	RolledBack  bool   // this start just performed an automatic rollback
	FailedStage bool   // the staged file failed verification/smoke test and was discarded
	Detail      string // one-line, user-facing explanation
}

// Bootstrap runs at the very top of main; it must be called before any port is bound or any database is opened.
//
// Three situations:
//
//	(1) a staged artex.new exists -> verify + smoke test; on success swap it in and ask for a restart, otherwise discard it and keep running the old version
//	(2) only the marker file is left -> we just swapped in, so count one attempt; roll back after enough consecutive failures
//	(3) nothing at all             -> normal start
func Bootstrap() (Action, State) {
	if os.Getenv(smokeEnv) != "" {
		return Continue, State{}
	}
	p, err := ResolvePaths()
	if err != nil {
		log.Printf("[update] skipping bootstrap: %v", err)
		return Continue, State{}
	}

	if _, err := os.Stat(p.New); err == nil {
		return applyStaged(p)
	}

	m, ok := readMarker(p.Marker)
	if !ok {
		return Continue, State{}
	}
	return confirmOrRollback(p, m)
}

// applyStaged handles the "a staged file exists" case: swap it in if it verifies, discard it otherwise.
//
// This is the only place in the whole upgrade path that overwrites the executable, and it is the last
// gate -- the smoke test catches corrupted downloads, the wrong architecture, missing dynamic
// libraries and the like. Once a binary that cannot start is let through, the supervisor script will
// tirelessly restart it, the Go code never gets to run, and automatic rollback becomes impossible.
func applyStaged(p Paths) (Action, State) {
	m, _ := readMarker(p.Marker)

	if err := verifyStaged(p); err != nil {
		log.Printf("[update] the staged new version failed verification and was discarded; continuing on the current version: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "new version failed verification and was discarded: " + err.Error()}
	}

	if err := swap(p); err != nil {
		log.Printf("[update] swap failed, continuing on the current version: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "swap failed: " + err.Error()}
	}

	// Swap succeeded. Keep the marker and let the next start (which runs the new version) confirm stability.
	m.Attempts = 0
	if m.StagedAt == 0 {
		m.StagedAt = time.Now().Unix()
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] failed to write the upgrade marker (automatic rollback is now unavailable): %v", err)
	}
	log.Printf("[update] swapped in %s, exiting to restart (exit %d)", orUnknown(m.To), ExitRestart)
	return Restart, State{Pending: true}
}

// confirmOrRollback handles "the start after a swap": count the attempts and put the old version back once the limit is exceeded.
//
// The counter only increments once the Go code is running, so it covers failures of the "executes but
// crashes during initialization" kind (incompatible config, port already taken, a blown DB migration);
// "cannot exec at all" is caught by the smoke test before the swap. Together the two are complete.
func confirmOrRollback(p Paths, m marker) (Action, State) {
	m.Attempts++
	if m.Attempts > maxAttempts {
		if err := rollback(p); err != nil {
			// If even the rollback fails, stop restarting or we end up in an infinite restart loop.
			// Clear the marker and let the process start in its current state -- if it cannot start,
			// at least the user can see why in the logs.
			log.Printf("[update] the new version failed to start %d times in a row and the rollback also failed: %v", maxAttempts, err)
			_ = os.Remove(p.Marker)
			return Continue, State{Detail: "the new version failed to start and the rollback also failed: " + err.Error()}
		}
		log.Printf("[update] the new version failed to start %d times in a row; rolled back to %s, exiting to restart (exit %d)",
			maxAttempts, orUnknown(m.From), ExitRestart)
		_ = os.Remove(p.Marker)
		return Restart, State{RolledBack: true, Detail: fmt.Sprintf("the new version failed to start; rolled back to %s", orUnknown(m.From))}
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] failed to update the upgrade marker: %v", err)
	}
	log.Printf("[update] the new version is starting (attempt %d/%d); the upgrade is confirmed once it runs stably",
		m.Attempts, maxAttempts)
	return Continue, State{Pending: true}
}

// Settle confirms the new version is running stably and clears the upgrade marker.
//
// main calls it on a delay once the HTTP listener is up: surviving that long is what counts.
// Otherwise the marker stays put and the next start keeps counting attempts until a rollback triggers.
func Settle() {
	p, err := ResolvePaths()
	if err != nil {
		return
	}
	settle(p)
}

func settle(p Paths) {
	if _, ok := readMarker(p.Marker); !ok {
		return // not a post-upgrade start, nothing to do
	}
	if err := os.Remove(p.Marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[update] failed to clear the upgrade marker: %v", err)
		return
	}
	log.Printf("[update] the new version is running stably, upgrade complete (the previous version is kept as %s)", p.Old)
}

// SettleDelay is how long the process must run before the new version counts as "survived".
const SettleDelay = 30 * time.Second

// verifyStaged verifies the staged file: first compare the SHA256, then actually run it once.
func verifyStaged(p Paths) error {
	want, err := os.ReadFile(p.Sum)
	if err != nil {
		return fmt.Errorf("read checksum: %w", err)
	}
	got, err := fileSHA256(p.New)
	if err != nil {
		return fmt.Errorf("compute checksum: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(want)), got) {
		return errors.New("SHA256 mismatch (corrupted download or tampering)")
	}
	return smokeTest(p.New)
}

// smokeTest starts the new binary with -h to confirm it really executes on this system.
// This catches a whole class of problems: truncated downloads, the wrong architecture (exec format error), missing dependencies.
func smokeTest(bin string) error {
	if err := os.Chmod(bin, 0o755); err != nil {
		return fmt.Errorf("make executable: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "-h")
	cmd.Env = append(os.Environ(), smokeEnv+"=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return errors.New("smoke test timed out (the new binary did not respond)")
	}
	if err != nil {
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "..."
		}
		return fmt.Errorf("smoke test failed: %v: %s", err, snippet)
	}
	return nil
}

// swap replaces the current binary with the staged new version.
//
// Both Unix and Windows allow renaming a running executable (Windows forbids deleting and
// overwriting, not renaming), so no per-platform handling is needed and we do not have to stop ourselves first.
func swap(p Paths) error {
	// rename on Windows does not overwrite an existing target, so a .old left by a previous upgrade must be cleared first.
	if err := os.Remove(p.Old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clean up the old backup %s: %w", p.Old, err)
	}
	if err := os.Rename(p.Current, p.Old); err != nil {
		return fmt.Errorf("back up the current version: %w", err)
	}
	if err := os.Rename(p.New, p.Current); err != nil {
		// The swap failed but the current version has already been moved aside; it must be put back
		// exactly as it was, or the next start has no executable.
		if rerr := os.Rename(p.Old, p.Current); rerr != nil {
			return fmt.Errorf("installing the new version failed (%v) and restoring the current version also failed: %w", err, rerr)
		}
		return fmt.Errorf("install the new version: %w", err)
	}
	_ = os.Remove(p.Sum)
	return nil
}

// rollback puts back the old version that swap backed up.
func rollback(p Paths) error {
	if _, err := os.Stat(p.Old); err != nil {
		return fmt.Errorf("no backup to roll back to at %s: %w", p.Old, err)
	}
	// Move the new version that would not start to .failed for diagnosis instead of deleting it.
	failed := p.Current + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(p.Current, failed); err != nil {
		return fmt.Errorf("move the failed version aside: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		return fmt.Errorf("restore the old version: %w", err)
	}
	return nil
}

// Rollback implements /api/update/rollback: deliberately go back to the previous version.
// It only swaps; restarting is again left to the supervisor script (the caller then exits with ExitRestart).
func Rollback() error {
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.Old); err != nil {
		return errors.New("no previous version to roll back to (" + p.Old + " does not exist)")
	}
	cleanStaged(p)
	if err := smokeTest(p.Old); err != nil {
		return fmt.Errorf("the previous version is not executable, refusing to roll back: %w", err)
	}
	// Swap current and backup: after a rollback you can roll forward again.
	tmp := p.Current + ".swap"
	_ = os.Remove(tmp)
	if err := os.Rename(p.Current, tmp); err != nil {
		return fmt.Errorf("move the current version aside: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		_ = os.Rename(tmp, p.Current)
		return fmt.Errorf("install the previous version: %w", err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		log.Printf("[update] failed to tidy up backups after the rollback (does not affect operation): %v", err)
	}
	_ = os.Remove(p.Marker)
	return nil
}

// HasBackup reports whether a previous version is available to roll back to, so the frontend can decide whether to show the rollback button.
func HasBackup() bool {
	p, err := ResolvePaths()
	if err != nil {
		return false
	}
	_, err = os.Stat(p.Old)
	return err == nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown version"
	}
	return s
}
