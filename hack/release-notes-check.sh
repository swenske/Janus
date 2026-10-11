#!/usr/bin/env bash
# Checks a release's hand-written notes against what it fixes (its
# security.json, hack/upstream security-notes): a release fixing
# vulnerabilities needs a "## 🔒" section, and that section must name the
# new version of each HAProxy branch or kernel track it fixes - such a fix
# concerns only the nodes on it. Prints the worst severity ("none" when it
# fixes nothing). Run when a release or pre-release is cut
# (image-build.yml) and again when a pre-release is promoted
# (release-promote.yml): its notes may have changed meanwhile.
#
# Usage: hack/release-notes-check.sh SECURITY.JSON NOTES.md [PREVIOUS VERSION]
#   PREVIOUS and VERSION only make the error say how to draft the section.
set -euo pipefail
security=$1 notes=$2 prev=${3:-PREVIOUS} version=${4:-VERSION}

severity=$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["max_severity"])' "$security")
if [ "$severity" != none ] && ! grep -q '^## 🔒' "$notes"; then
  echo "::error::$version fixes vulnerabilities since $prev (up to $severity) but its notes have no '## 🔒' section - draft it with: make upstream-security-notes FROM=$prev RELEASE=$version" >&2
  exit 1
fi
python3 - "$security" "$notes" <<'PY'
import json, sys
doc = json.load(open(sys.argv[1]))
notes = open(sys.argv[2]).read()
section = notes[notes.find("## 🔒"):] if "## 🔒" in notes else ""
missing = [f'{u["title"]} {u["to"]}' for u in doc.get("updates") or []
           if u.get("component") and u.get("fixes") and u["to"] not in section]
if missing:
    print("::error::the notes' 🔒 section doesn't say which nodes these fixes concern - name their versions: " + ", ".join(missing), file=sys.stderr)
    sys.exit(1)
PY
echo "$severity"
