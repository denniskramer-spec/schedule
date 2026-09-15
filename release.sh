#!/bin/sh
# Schedule - publish the built installer as a GitHub release.
#
#   ./release.sh            publish the version in latest.json to denniskramer-spec/schedule
#   REPO=you/repo ./release.sh
#
# Needs the GitHub CLI signed in ("gh auth login"), or GITHUB_TOKEN set to a
# token with repo access. Creates the repository if it does not exist, pushes
# the current commit, tags it v<version>, and attaches Schedule-Setup.exe and
# latest.json - the two files installed copies fetch. Run ./build.sh installer
# first.
set -e
cd "$(dirname "$0")"

REPO="${REPO:-denniskramer-spec/schedule}"
VERSION=$(python3 -c 'import json;print(json.load(open("latest.json"))["version"])')
NOTES=$(python3 -c 'import json;print(json.load(open("latest.json"))["notes"])')
TAG="v$VERSION"

for f in Schedule-Setup.exe latest.json; do
  [ -f "$f" ] || { echo "$f is missing: run ./build.sh installer first" >&2; exit 2; }
done
[ "$(sha256sum Schedule-Setup.exe | cut -d' ' -f1)" = \
  "$(python3 -c 'import json;print(json.load(open("latest.json"))["sha256"])')" ] \
  || { echo "latest.json does not match Schedule-Setup.exe: rebuild" >&2; exit 2; }

if command -v gh >/dev/null 2>&1; then
  gh auth status >/dev/null 2>&1 || { echo "gh is not signed in: run  gh auth login" >&2; exit 2; }
  if ! gh repo view "$REPO" >/dev/null 2>&1; then
    echo "creating $REPO (public, so installed copies can fetch releases)"
    gh repo create "$REPO" --public --source=. --remote=origin --push
  else
    git remote get-url origin >/dev/null 2>&1 || git remote add origin "https://github.com/$REPO.git"
    git push -u origin HEAD
  fi
  gh release create "$TAG" Schedule-Setup.exe latest.json \
    --repo "$REPO" --title "Schedule $VERSION" --notes "$NOTES" --latest
  echo "published: https://github.com/$REPO/releases/tag/$TAG"
  exit 0
fi

[ -n "$GITHUB_TOKEN" ] || { echo "install the GitHub CLI and run  gh auth login, or set GITHUB_TOKEN" >&2; exit 2; }
API="https://api.github.com"
auth="Authorization: Bearer $GITHUB_TOKEN"
if [ "$(curl -s -o /dev/null -w '%{http_code}' -H "$auth" "$API/repos/$REPO")" != 200 ]; then
  echo "creating $REPO"
  curl -sf -H "$auth" "$API/user/repos" -d "{\"name\":\"${REPO#*/}\",\"private\":false}" >/dev/null
fi
git remote get-url origin >/dev/null 2>&1 || git remote add origin "https://github.com/$REPO.git"
git push -u "https://x-access-token:$GITHUB_TOKEN@github.com/$REPO.git" HEAD:main
rel=$(curl -sf -H "$auth" "$API/repos/$REPO/releases" \
  -d "$(python3 -c "import json,sys;print(json.dumps({'tag_name':'$TAG','target_commitish':'main','name':'Schedule $VERSION','body':sys.argv[1],'make_latest':'true'}))" "$NOTES")")
id=$(printf '%s' "$rel" | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')
for f in Schedule-Setup.exe latest.json; do
  case $f in *.exe) type=application/octet-stream;; *) type=application/json;; esac
  curl -sf -H "$auth" -H "Content-Type: $type" --data-binary "@$f" \
    "https://uploads.github.com/repos/$REPO/releases/$id/assets?name=$f" >/dev/null
  echo "uploaded $f"
done
echo "published: https://github.com/$REPO/releases/tag/$TAG"
