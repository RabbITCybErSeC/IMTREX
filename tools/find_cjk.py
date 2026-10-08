#!/usr/bin/env python3
"""Deterministic CJK (Chinese/Japanese) character scanner for the IMTREX repository.

Scans tracked text files for CJK code points and non-UTF-8 bytes, reports every
occurrence as file:line, and (in --check mode) exits non-zero while unallowlisted
occurrences remain. This is the single source of truth for the translation effort:
no manual scanning, identical output for an identical tree.

Design goals:
- Deterministic: the file list comes from `git ls-files` (sorted); output is sorted.
- Zero dependencies: Python 3.8+ standard library only.
- Reviewable policy: every tolerated occurrence must be either listed in
  tools/cjk-allowlist.json with a written reason, or marked inline on the line
  itself with the marker "cjk-allow".

Usage:
  python3 tools/find_cjk.py                     # human-readable report
  python3 tools/find_cjk.py --stats             # inventory summary
  python3 tools/find_cjk.py --check             # CI gate; exit 1 on violations
  python3 tools/find_cjk.py --json              # machine-readable output
  python3 tools/find_cjk.py --path web/src      # limit to a subdirectory
  python3 tools/find_cjk.py --show-allowed      # also list allowlisted lines

Allowlist file format (tools/cjk-allowlist.json):
  {
    "entries": [
      {"path": "db/schema.sql", "line": "\\\\[\\u6a21\\u578b\\\\]", "reason": "legacy DB prefix"}
    ]
  }
  - "path" is a glob matched against the repo-relative path ("**" crosses
    directories; a bare path with no "line" also tolerates a CJK file name).
  - "line" is an optional regular expression; when present, only matching lines
    are tolerated.
  - "reason" is required and documents why the occurrence must stay.

Exit codes: 0 = clean (or only allowlisted findings), 1 = violations remain.
"""

import argparse
import json
import os
import re
import subprocess
import sys
from collections import Counter

# ---------------------------------------------------------------------------
# CJK code point ranges (Han ideographs, kana, CJK punctuation, fullwidth forms)
# ---------------------------------------------------------------------------
CJK_RANGES = [
    (0x2E80, 0x2EFF),    # CJK Radicals Supplement
    (0x3000, 0x303F),    # CJK Symbols and Punctuation (ideographic comma/period, brackets)
    (0x3040, 0x309F),    # Hiragana
    (0x30A0, 0x30FF),    # Katakana
    (0x3100, 0x312F),    # Bopomofo
    (0x31A0, 0x31BF),    # Bopomofo Extended
    (0x31F0, 0x31FF),    # Katakana Phonetic Extensions
    (0x3400, 0x4DBF),    # CJK Unified Ideographs Extension A
    (0x4E00, 0x9FFF),    # CJK Unified Ideographs
    (0xF900, 0xFAFF),    # CJK Compatibility Ideographs
    (0xFE30, 0xFE4F),    # CJK Compatibility Forms
    (0xFF00, 0xFFEF),    # Halfwidth and Fullwidth Forms (incl. halfwidth kana)
    (0x20000, 0x2A6DF),  # CJK Unified Ideographs Extension B
    (0x2A700, 0x2EBEF),  # CJK Unified Ideographs Extensions C-F
    (0x2F800, 0x2FA1F),  # CJK Compatibility Ideographs Supplement
]

CJK_RE = re.compile(
    "[" + "".join("\\U%08x-\\U%08x" % (lo, hi) for lo, hi in CJK_RANGES) + "]")

# Heuristic: does the line look like a comment in a common language?
COMMENT_RE = re.compile(r"^\s*(//|/\*|\*|#|<!--|;|--\b|rem\b)", re.IGNORECASE)

# Inline escape hatch: a line carrying this marker is reported as allowed.
INLINE_RE = re.compile(r"cjk-allow", re.IGNORECASE)

SKIP_DIRS = {".git", "node_modules", ".next", "out", "dist", "__pycache__", "data"}
MAX_SNIPPET = 160
BINARY_SNIFF = 8192


def char_kind(ch):
    """Classify one CJK character: kana, bopomofo, punct, fullwidth or han."""
    cp = ord(ch)
    if 0x3040 <= cp <= 0x309F or 0x30A0 <= cp <= 0x30FF or 0x31F0 <= cp <= 0x31FF:
        return "kana"
    if 0x3100 <= cp <= 0x312F or 0x31A0 <= cp <= 0x31BF:
        return "bopomofo"
    if 0x2E80 <= cp <= 0x2EFF or 0x3000 <= cp <= 0x303F or 0xFE30 <= cp <= 0xFE4F:
        return "punct"
    if 0xFF00 <= cp <= 0xFFEF:
        return "fullwidth"
    return "han"


def cjk_kinds(line):
    """Counter of the CJK kinds present in one line."""
    return Counter(char_kind(ch) for ch in line if CJK_RE.match(ch))


def glob_to_regex(glob):
    """Translate a path glob to a compiled regex ('**' crosses directories)."""
    i, n = 0, len(glob)
    out = ["^"]
    while i < n:
        c = glob[i]
        if c == "*":
            if glob[i:i + 2] == "**":
                i += 2
                if i < n and glob[i] == "/":
                    out.append("(?:.*/)?")  # '**/' also matches zero directories
                    i += 1
                else:
                    out.append(".*")
                continue
            out.append("[^/]*")
        elif c == "?":
            out.append("[^/]")
        else:
            out.append(re.escape(c))
        i += 1
    out.append("$")
    return re.compile("".join(out))


def load_allowlist(path):
    if not path or not os.path.isfile(path):
        return []
    with open(path, "r", encoding="utf-8") as fh:
        data = json.load(fh)
    entries = []
    for e in data.get("entries", []):
        if "path" not in e or "reason" not in e:
            raise SystemExit("allowlist entry needs 'path' and 'reason': %r" % (e,))
        entries.append({
            "path_re": glob_to_regex(e["path"]),
            "line_re": re.compile(e["line"]) if e.get("line") else None,
            "reason": e["reason"],
        })
    return entries


def allow_reason(entries, path, line):
    """Return the reason string when (path, line) is allowlisted, else None.

    A line of None checks file-level entries only (used for CJK file names and
    non-UTF-8 files).
    """
    for e in entries:
        if not e["path_re"].match(path):
            continue
        if e["line_re"] is None:
            return e["reason"]
        if line is not None and e["line_re"].search(line):
            return e["reason"]
    return None


def file_level_allowed(entries, rel, text):
    """Build the allowed-entry record for a file-level finding, or None."""
    reason = allow_reason(entries, rel, None)
    if not reason:
        return None
    return {"path": rel, "line": 0, "reason": reason, "text": text,
            "kinds": {}, "likely_comment": False}


def list_files_git(root, include_untracked):
    args = ["git", "-C", root, "ls-files", "-z"]
    if include_untracked:
        args += ["--others", "--exclude-standard"]
    try:
        out = subprocess.run(args, capture_output=True, check=False)
    except OSError:
        return None
    if out.returncode != 0:
        return None
    return [b.decode("utf-8", "surrogateescape")
            for b in out.stdout.split(b"\0") if b]


def list_files_walk(root):
    found = []
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
        for name in filenames:
            full = os.path.join(dirpath, name)
            found.append(os.path.relpath(full, root).replace(os.sep, "/"))
    return sorted(found)


def path_selected(path, filters):
    for f in filters:
        f = f.rstrip("/")
        if path == f or path.startswith(f + "/"):
            return True
    return False


def apply_path_filter(paths, filters):
    if not filters:
        return paths
    return [p for p in paths if path_selected(p, filters)]


def scan_text(text):
    """Yield (lineno, line, kinds_counter) for every line containing CJK."""
    for i, line in enumerate(text.splitlines(), 1):
        if CJK_RE.search(line):
            yield i, line, cjk_kinds(line)


def scan_file(root, rel, entries):
    """Scan one file; returns (violations, allowed, non_utf8)."""
    full = os.path.join(root, rel)
    violations, allowed, non_utf8 = [], [], None
    try:
        with open(full, "rb") as fh:
            raw = fh.read()
    except OSError as exc:
        return violations, allowed, "unreadable: %s" % exc
    if b"\0" in raw[:BINARY_SNIFF]:
        return violations, allowed, non_utf8  # binary, skip
    try:
        text = raw.decode("utf-8")
    except UnicodeDecodeError as exc:
        non_utf8 = "invalid UTF-8 at byte %d (possible GBK/Shift-JIS text)" % exc.start
        text = raw.decode("utf-8", "replace")
    for lineno, line, kinds in scan_text(text):
        item = {
            "path": rel,
            "line": lineno,
            "kinds": dict(kinds),
            "likely_comment": bool(COMMENT_RE.match(line)),
            "text": line.strip()[:MAX_SNIPPET],
        }
        if INLINE_RE.search(line):
            item["reason"] = "inline cjk-allow marker"
            allowed.append(item)
            continue
        reason = allow_reason(entries, rel, line)
        if reason:
            item["reason"] = reason
            allowed.append(item)
        else:
            violations.append(item)
    return violations, allowed, non_utf8


def scan_tree(root, paths, entries):
    """Scan every path; returns the finding lists and the scanned-file count."""
    violations, allowed, path_violations, non_utf8 = [], [], [], []
    for rel in paths:
        if CJK_RE.search(rel):
            entry = file_level_allowed(entries, rel, "(file name)")
            if entry:
                allowed.append(entry)
            else:
                path_violations.append(
                    {"path": rel, "reason": "CJK characters in file name"})
        v, a, nu = scan_file(root, rel, entries)
        violations += v
        allowed += a
        if nu:
            entry = file_level_allowed(entries, rel, nu)
            if entry:
                allowed.append(entry)
            else:
                non_utf8.append({"path": rel, "reason": nu})
    return violations, allowed, path_violations, non_utf8


def build_summary(paths, violations, path_violations, non_utf8, allowed):
    by_file = Counter(v["path"] for v in violations)
    kinds_total = Counter()
    for item in violations:
        kinds_total.update(item["kinds"])
    summary = {
        "files_total": len(paths),
        "files_scanned": len(paths),
        "violation_lines": len(violations),
        "violation_files": len(by_file),
        "path_violations": len(path_violations),
        "non_utf8_files": len(non_utf8),
        "allowed_lines": len(allowed),
        "kinds": dict(kinds_total),
    }
    return summary, by_file, kinds_total


def render_report(out, args, source, summary, kinds_total,
                  violations, path_violations, non_utf8, allowed):
    out.write("IMTREX CJK scan (%s: %d files, %d scanned)\n" % (
        source, summary["files_total"], summary["files_scanned"]))
    out.write(
        "violations: %d lines in %d files | path names: %d | non-UTF-8: %d"
        " | allowed: %d\n" % (
            summary["violation_lines"], summary["violation_files"],
            summary["path_violations"], summary["non_utf8_files"],
            summary["allowed_lines"]))
    if kinds_total:
        out.write("kinds: %s\n" % ", ".join(
            "%s=%d" % kv for kv in sorted(kinds_total.items())))
    out.write("\n")

    grouped = {}
    for item in violations:
        grouped.setdefault(item["path"], []).append(item)
    for path, items in grouped.items():
        out.write("%s  (%d lines)\n" % (path, len(items)))
        limit = len(items) if args.max_lines_per_file == 0 else min(
            len(items), args.max_lines_per_file)
        for item in items[:limit]:
            marker = "comment" if item["likely_comment"] else "string/code"
            out.write("  %5d  [%s] %s\n" % (item["line"], marker, item["text"]))
        if len(items) > limit:
            out.write("  ... %d more (--max-lines-per-file 0 to show all)\n" % (
                len(items) - limit))
        out.write("\n")

    for item in path_violations:
        out.write("PATH NAME  %s  (%s)\n" % (item["path"], item["reason"]))
    for item in non_utf8:
        out.write("NON-UTF8   %s  (%s)\n" % (item["path"], item["reason"]))

    if args.show_allowed and allowed:
        out.write("\n-- allowed (suppressed) --\n")
        for item in allowed:
            out.write("  %s:%s  (%s) %s\n" % (
                item["path"], item["line"], item["reason"], item["text"]))


def render_stats(out, violations, allowed):
    by_dir = Counter()
    by_ext = Counter()
    by_file = Counter()
    for item in violations:
        path = item["path"]
        by_file[path] += 1
        by_dir[path.split("/", 1)[0] if "/" in path else "."] += 1
        by_ext[os.path.splitext(path)[1] or "(none)"] += 1

    out.write("by top-level dir:\n")
    for d, n in by_dir.most_common():
        out.write("  %-14s %4d lines\n" % (d, n))
    out.write("by extension:\n")
    for e, n in by_ext.most_common(12):
        out.write("  %-14s %4d lines\n" % (e, n))
    out.write("top files:\n")
    for path, n in by_file.most_common(20):
        out.write("  %4d  %s\n" % (n, path))
    if allowed:
        out.write("allowed by reason:\n")
        for reason, n in Counter(a["reason"] for a in allowed).most_common():
            out.write("  %4d  %s\n" % (n, reason))


def main():
    ap = argparse.ArgumentParser(description="Scan the repo for CJK characters.")
    ap.add_argument("--root", default=".", help="repository root (default: cwd)")
    ap.add_argument("--path", action="append", default=[],
                    help="limit the scan to a subdirectory (repeatable)")
    ap.add_argument("--json", action="store_true", help="machine-readable output")
    ap.add_argument("--check", action="store_true",
                    help="exit 1 when unallowlisted occurrences remain")
    ap.add_argument("--stats", action="store_true", help="inventory summary only")
    ap.add_argument("--show-allowed", action="store_true",
                    help="also list allowlisted occurrences")
    ap.add_argument("--allowlist", default=None,
                    help="allowlist JSON (default: tools/cjk-allowlist.json)")
    ap.add_argument("--no-allowlist", action="store_true",
                    help="ignore the allowlist entirely (raw scan)")
    ap.add_argument("--no-git", action="store_true",
                    help="walk the tree instead of using git ls-files")
    ap.add_argument("--include-untracked", action="store_true",
                    help="also scan untracked files (git mode)")
    ap.add_argument("--max-lines-per-file", type=int, default=8,
                    help="detail lines per file in the report (0 = all)")
    args = ap.parse_args()

    root = os.path.abspath(args.root)
    if args.no_git:
        paths, source = list_files_walk(root), "directory walk"
    else:
        paths = list_files_git(root, args.include_untracked)
        source = "git ls-files"
        if paths is None:
            paths, source = list_files_walk(root), "directory walk (git unavailable)"
    paths = sorted(set(apply_path_filter(paths, args.path)))

    allowlist_path = args.allowlist or os.path.join(root, "tools", "cjk-allowlist.json")
    entries = [] if args.no_allowlist else load_allowlist(allowlist_path)

    violations, allowed, path_violations, non_utf8 = scan_tree(root, paths, entries)
    summary, _by_file, kinds_total = build_summary(
        paths, violations, path_violations, non_utf8, allowed)

    if args.json:
        json.dump({
            "source": source,
            "summary": summary,
            "violations": violations,
            "path_violations": path_violations,
            "non_utf8": non_utf8,
            "allowed": allowed if args.show_allowed else [],
        }, sys.stdout, indent=2, ensure_ascii=True)
        sys.stdout.write("\n")
        if args.check:
            return 1 if violations or path_violations or non_utf8 else 0
        return 0

    out = sys.stdout
    if not args.stats:
        render_report(out, args, source, summary, kinds_total,
                      violations, path_violations, non_utf8, allowed)
    if args.stats or args.check:
        render_stats(out, violations, allowed)

    if args.check:
        bad = len(violations) + len(path_violations) + len(non_utf8)
        if bad:
            out.write("\nFAIL: %d unallowlisted finding(s) remain\n" % bad)
            return 1
        out.write("\nOK: no unallowlisted CJK or non-UTF-8 findings\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
