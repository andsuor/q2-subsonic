#!/bin/sh
# Print one version's section of CHANGELOG.md, without its heading: the release notes.
#   contrib/changelog-section.sh X.Y|Unreleased [FILE|-]
# Exits 1 when the section is missing or has no "- " entry (it still prints what it found).
# Used by contrib/release.sh and .github/workflows/{ci,release}.yml.
set -eu
[ $# -ge 1 ] || { echo "usage: $0 X.Y|Unreleased [FILE|-]" >&2; exit 2; }
f=${2:-$(dirname "$0")/../CHANGELOG.md}
awk -v head="## [$1]" '
    index($0, "## [") == 1 { if (on) exit; on = index($0, head) == 1; next }
    !on { next }
    /^\[[^]]+\]: / { exit }                          # the link references after the last section
    NF { while (blank) { print ""; blank-- } print; seen = 1; if (/^- /) entry = 1; next }
    seen { blank++ }                                 # keep blank lines between entries, not around them
    END { exit !entry }
' "$f"
