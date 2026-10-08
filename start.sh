#!/bin/sh
# ARTEX supervisor start script (Linux / macOS / Docker ENTRYPOINT)
#
# Usage:
#   ./start.sh                       run in the foreground (Ctrl-C to stop)
#   nohup ./start.sh >artex.log 2>&1 &   run in the background
#   ./start.sh -addr :9000           extra flags are passed through to artex verbatim
#
# It does exactly one thing: start artex, and when the process exits, decide from the exit code whether to start it again.
#
#   0      stopped normally by the user  -> leave the loop
#   75     the program asked to restart  -> rerun immediately ("one-click update" or "roll back" was clicked in the UI)
#   other  crash                         -> rerun after a backoff (1->2->4... capped at 60 seconds)
#
# Downloading, SHA256 verification and swapping the binary are deliberately not done here: that
# logic would have to be written twice (sh and bat), and it is exactly the part that must never go
# wrong -- once a binary that cannot start is swapped in, this script would faithfully restart it
# over and over and the user would have to rescue the machine by hand. So verification and swapping
# all live in Go (the selfupdate package) and are done by artex itself at startup; this script stays dumb.
set -u

cd "$(dirname "$0")" || exit 1

BIN=./artex
[ -x "$BIN" ] || { echo "[artex] executable not found: $BIN" >&2; exit 1; }

RESTART_CODE=75
MAX_DELAY=60

child=0
stopping=0

# Forward the stop signal to artex itself.
#
# This is required under Docker: docker stop only sends SIGTERM to PID 1 (this script), never to
# child processes. Without forwarding, artex never receives the signal, cannot shut down gracefully,
# and is hard-killed by SIGKILL after 10 seconds, cutting running tasks off mid-flight.
forward() {
	stopping=1
	if [ "$child" -ne 0 ]; then
		kill -TERM "$child" 2>/dev/null || true
	fi
}
trap forward INT TERM

delay=1
while :; do
	"$BIN" "$@" &
	child=$!

	# A signal interrupts wait and makes it return >128. At that point the child is still shutting
	# down gracefully, so we have to wait once more to get its real exit code.
	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0

	if [ "$stopping" -eq 1 ]; then
		echo "[artex] stopped"
		exit 0
	fi

	case "$code" in
		0)
			echo "[artex] exited normally"
			exit 0
			;;
		"$RESTART_CODE")
			# An update/rollback is staged: on the next run artex completes the swap at startup (see selfupdate.Bootstrap).
			echo "[artex] restart requested (applying the new version)..."
			delay=1
			;;
		*)
			echo "[artex] abnormal exit (code=$code), restarting in ${delay}s" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
