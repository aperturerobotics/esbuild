#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

# Parse current version
current=$(<version.txt)
[[ "$current" =~ ^aperture-([0-9]+)$ ]] || { echo "error: unexpected version.txt: $current" >&2; exit 1; }
old="aperture-${BASH_REMATCH[1]}"
new="aperture-$(( BASH_REMATCH[1] + 1 ))"

# Preflight: clean working tree, on aperture branch
branch=$(git branch --show-current)
[[ "$branch" == "aperture" ]] || { echo "error: expected branch 'aperture', on '$branch'" >&2; exit 1; }
git diff --quiet && git diff --cached --quiet || { echo "error: working tree not clean" >&2; git status -s; exit 1; }

echo "${old} -> ${new}"

# Update all version references
files=(version.txt cmd/esbuild/version.go npm/esbuild/package.json)
for f in "${files[@]}"; do sed -i'' -e "s/${old}/${new}/g" "$f"; done

# Verify no stale references
stale=$(grep -rl "${old}" "${files[@]}" 2>/dev/null || true)
[[ -z "$stale" ]] || { echo "error: stale ${old} in: ${stale}" >&2; exit 1; }

# Show what changed
git diff --stat

# Commit, tag, push
git add "${files[@]}"
git commit -sm "release: ${new}"
git tag "${new}"
git push
git push origin "${new}"

echo "done: ${new}"
