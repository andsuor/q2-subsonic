#!/usr/bin/env bash
# Cut release q2-vX.Y from master: CHANGELOG.md's Unreleased entries become "## [X.Y] - <today>", the
# link references and cmd/q2-subsonic/main.go's version follow, and the commit "release: q2-vX.Y" gets
# the annotated tag q2-vX.Y. Pushing the tag publishes the release (.github/workflows/release.yml).
#   contrib/release.sh X.Y [--dry-run] [--push]
# --dry-run runs every check and test and prints the notes and the diff, but changes nothing.
# --push pushes master and the tag at the end.
set -euo pipefail
die() { echo "release.sh: $*" >&2; exit 1; }
cd "$(dirname "$0")/.."

v='' dry='' push=''
for a in "$@"; do
    case $a in
    --dry-run) dry=1 ;;
    --push) push=1 ;;
    *) [ -z "$v" ] || die "one version only"; v=$a ;;
    esac
done
[[ $v =~ ^[0-9]+\.[0-9]+$ ]] || die "usage: contrib/release.sh X.Y [--dry-run] [--push]"
tag=q2-v$v today=$(date +%F)

[ "$(git branch --show-current)" = master ] || die "not on master"
[ -z "$(git status --porcelain)" ] || die "the tree has uncommitted changes"
git fetch -q --tags origin master
[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/master)" ] || die "master is not origin/master; pull or push first"
if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then die "tag $tag exists"; fi
contrib/changelog-section.sh Unreleased >/dev/null || die "CHANGELOG.md has no entries under ## [Unreleased]"

go vet ./...
go test ./...
python3 contrib/shanlingq2/test_launcher.py

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
awk -v v="$v" -v d="$today" '
    $0 == "## [Unreleased]" { print; print ""; print "## [" v "] - " d; next }
    /^\[Unreleased\]: .*\/compare\/q2-v[0-9.]+\.\.\.HEAD$/ {
        base = prev = $2
        sub(/\/compare\/.*/, "", base)
        sub(/.*\/compare\//, "", prev); sub(/\.\.\.HEAD$/, "", prev)
        print "[Unreleased]: " base "/compare/q2-v" v "...HEAD"
        print "[" v "]: " base "/compare/" prev "...q2-v" v
        next
    }
    { print }
' CHANGELOG.md >"$tmp/CHANGELOG.md"
sed "s/^const version = \"q2-v[0-9.]*\"\$/const version = \"$tag\"/" cmd/q2-subsonic/main.go >"$tmp/main.go"
grep -q "^## \[$v\] - $today\$" "$tmp/CHANGELOG.md" && grep -q "^\[$v\]: .*\.\.\.$tag\$" "$tmp/CHANGELOG.md" ||
    die "CHANGELOG.md needs a '## [Unreleased]' heading and an '[Unreleased]: …/compare/q2-vA.B...HEAD' link"
grep -q "^const version = \"$tag\"\$" "$tmp/main.go" || die "no 'const version = \"q2-v…\"' in cmd/q2-subsonic/main.go"

echo "== Release notes for $tag"
contrib/changelog-section.sh "$v" "$tmp/CHANGELOG.md"
if [ -n "$dry" ]; then
    echo "== Changes"
    diff -u CHANGELOG.md "$tmp/CHANGELOG.md" || true
    diff -u cmd/q2-subsonic/main.go "$tmp/main.go" || true
    echo "== Dry run: nothing committed, tagged or pushed"
    exit 0
fi

cp "$tmp/CHANGELOG.md" CHANGELOG.md
cp "$tmp/main.go" cmd/q2-subsonic/main.go
git commit -q -m "release: $tag" CHANGELOG.md cmd/q2-subsonic/main.go
git tag -a --cleanup=verbatim "$tag" -m "q2-subsonic v$v" -m "$(contrib/changelog-section.sh "$v")" # verbatim keeps "### Added"
if [ -n "$push" ]; then
    git push --atomic origin master "$tag"
else
    echo "== Committed and tagged $tag. Publish with: git push --atomic origin master $tag"
fi
