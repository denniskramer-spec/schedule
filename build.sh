#!/bin/sh
# Schedule - build helper. Same targets as the Makefile, for machines without make.
#
#   ./build.sh run       test locally on Linux
#   ./build.sh windows   cross-compile schedule.exe for Windows
#   ./build.sh installer build Schedule-Setup.exe, schedule.exe carried inside
#   ./build.sh icons     rebuild the icon and version info baked into both exes
#   ./build.sh test      run the unit tests
#   ./build.sh uitest    drive the real app in a headless Chrome (needs Chrome)
#   ./build.sh vet       static checks
#
# UPDATE_URL is stamped into the exe as where latest.json is published; the
# default is the GitHub release. Installed copies check it for newer versions.
set -e
cd "$(dirname "$0")"

case "${1:-windows}" in
  run)
    go run .
    ;;
  windows)
    CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
      go build -trimpath -ldflags="-s -w -H windowsgui -X main.version=${VERSION:-2.4} -X main.defaultUpdateURL=${UPDATE_URL:-https://github.com/denniskramer-spec/schedule/releases/latest/download/latest.json}" -o schedule.exe .
    ls -lh schedule.exe
    ;;
  installer)
    "$0" windows
    cp schedule.exe installer/payload/schedule.exe
    CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
      go build -trimpath -ldflags="-s -w -H windowsgui -X main.version=${VERSION:-2.4}" \
      -o Schedule-Setup.exe ./installer/
    # What the app's update check reads; upload it next to Schedule-Setup.exe.
    # The sha256 is what the app checks the download against before running it.
    sum=$(sha256sum Schedule-Setup.exe | cut -d' ' -f1)
    printf '{"version": "%s", "file": "Schedule-Setup.exe", "sha256": "%s", "notes": "%s"}\n' \
      "${VERSION:-2.4}" "$sum" "${NOTES:-}" > latest.json
    ls -lh Schedule-Setup.exe latest.json
    ;;
  icons)
    # go-winres turns icon.ico into .syso files, which "go build" links into
    # the Windows exes on its own. Only needed again if icon.ico changes.
    winres="go run github.com/tc-hib/go-winres@v0.3.3"
    $winres simply --arch amd64,arm64 --manifest gui --icon icon.ico \
      --product-name Schedule --file-description Schedule \
      --product-version "${VERSION:-2.4}" --file-version "${VERSION:-2.4}" \
      --original-filename schedule.exe --out rsrc
    (cd installer && $winres simply --arch amd64,arm64 --manifest gui --icon ../icon.ico \
      --product-name Schedule --file-description "Schedule Setup" \
      --product-version "${VERSION:-2.4}" --file-version "${VERSION:-2.4}" \
      --original-filename Schedule-Setup.exe --out rsrc)
    ls rsrc_*.syso installer/rsrc_*.syso
    ;;
  test)
    go test ./...
    ;;
  uitest)
    go test -tags uitest -count=1 -v ./uitest/
    ;;
  vet)
    go vet ./...
    ;;
  *)
    echo "usage: $0 {run|windows|installer|icons|test|uitest|vet}" >&2
    exit 2
    ;;
esac
