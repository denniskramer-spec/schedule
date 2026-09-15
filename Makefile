# Schedule - build targets
#
# make run       test locally on Linux
# make windows   cross-compile schedule.exe for Windows
# make installer build Schedule-Setup.exe, which carries schedule.exe inside it
# make test      run the unit tests
# make uitest    drive the real app in a headless Chrome (needs Chrome)
# make vet       static checks
# make clean     remove build output

BINARY := schedule.exe
SETUP  := Schedule-Setup.exe
VERSION := 2.5
# Where latest.json is published (https); stamped into the exe when set.
UPDATE_URL ?= https://github.com/denniskramer-spec/schedule/releases/latest/download/latest.json

.PHONY: run windows installer test uitest vet clean

run:
	go run .

windows:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
	  go build -trimpath -ldflags="-s -w -H windowsgui -X main.version=$(VERSION) -X main.defaultUpdateURL=$(UPDATE_URL)" -o $(BINARY) .
	@ls -lh $(BINARY)

installer: windows
	cp $(BINARY) installer/payload/$(BINARY)
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
	  go build -trimpath -ldflags="-s -w -H windowsgui -X main.version=$(VERSION)" \
	  -o $(SETUP) ./installer/
	@printf '{"version": "%s", "file": "Schedule-Setup.exe", "sha256": "%s", "notes": "%s"}\n' \
	  "$(VERSION)" "$$(sha256sum $(SETUP) | cut -d' ' -f1)" "$(NOTES)" > latest.json
	@ls -lh $(SETUP) latest.json

test:
	go test ./...

uitest:
	go test -tags uitest -count=1 -v ./uitest/

vet:
	go vet ./...

clean:
	rm -f $(BINARY) $(SETUP) latest.json installer/payload/$(BINARY)
