package agent

import (
	"log"
	"path/filepath"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/noaadapter"
)

// noaWarn returns a diagnostics sink tagging non-fatal noa messages with the
// session, routed through the package logger (agents have no per-instance one).
func noaWarn(session string) func(string) {
	return func(msg string) { log.Printf("[noa] %s: %s", session, msg) }
}

// noa is the "model-driven context compaction" mechanism introduced in norma v0.4.0, exposed as an
// experimental platform feature the user toggles in the system settings. It is mutually exclusive with
// the builtin compaction: noaadapter.Enable is the only entry point and attaches the context takeover
// (Compactor), the Compress tool and three resident prompt sections in one go; not calling Enable means
// it is off (and the builtin compaction works as usual). The switch is resolved by the noaEnabledFn each
// agent injects, read once per run, so toggling it only affects runs started afterwards and no agent needs rebuilding.

// enableNoa wires noa into opts when the resolver reports it enabled. archiveRoot is the persistence base
// directory for the compressed originals (the global workDir, so every agent lands under <workDir>/noa
// rather than being scattered across task/intent directories), and sessionID
// names the archive subdirectory under it (it is globally unique, so there is no clash within one base directory).
//
// noa is experimental: a wiring failure must never interrupt a real task. On an error it is reported through onWarn and falls back to the builtin compaction.
// On success opts.Compaction is cleared so agentcore does not warn about "two context managers set at once".
func enableNoa(opts *agentcore.Options, enabled func() bool, archiveRoot, sessionID string, onWarn func(string)) {
	if enabled == nil || !enabled() {
		return
	}
	if opts.OnWarn == nil {
		opts.OnWarn = onWarn
	}
	if err := noaadapter.Enable(opts, noaadapter.Options{
		ArchiveBaseDir: filepath.Join(archiveRoot, "noa"),
		SessionID:      sessionID,
		OnWarn:         onWarn,
	}); err != nil {
		if onWarn != nil {
			onWarn("enabling noa compaction failed, falling back to the builtin compaction: " + err.Error())
		}
		return
	}
	// Compactor overrides Compaction, but agentcore warns every time both are set; clear it explicitly.
	opts.Compaction = nil
}
