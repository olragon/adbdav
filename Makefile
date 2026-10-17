BINARY  := adbdav
APP     := Adbdav.app
VERSION ?= 0.1.0
PREFIX  ?= /opt/homebrew

.PHONY: all build check fmt-check test vet fmt icon app bench install uninstall clean

all: check build

# Everything CI should run, and everything that needs no phone attached.
check: fmt-check vet test

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BINARY) .

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

# Regenerate the menu bar glyph from its source geometry.
icon:
	go run ./internal/ui/gen

# Reproduce the numbers in the README. Needs a device attached; read-only.
bench: build
	./bench/bench.sh -o bench/RESULTS.md

# A menu bar app needs a bundle: LSUIElement keeps it out of the Dock and the
# app switcher, which is what makes it a status-bar item rather than a window.
app: build
	rm -rf $(APP)
	mkdir -p $(APP)/Contents/MacOS
	cp $(BINARY) $(APP)/Contents/MacOS/$(BINARY)
	printf '%s\n' \
	  '<?xml version="1.0" encoding="UTF-8"?>' \
	  '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">' \
	  '<plist version="1.0"><dict>' \
	  '  <key>CFBundleName</key><string>Adbdav</string>' \
	  '  <key>CFBundleDisplayName</key><string>Adbdav</string>' \
	  '  <key>CFBundleIdentifier</key><string>net.erpfit.adbdav</string>' \
	  '  <key>CFBundleVersion</key><string>$(VERSION)</string>' \
	  '  <key>CFBundleShortVersionString</key><string>$(VERSION)</string>' \
	  '  <key>CFBundlePackageType</key><string>APPL</string>' \
	  '  <key>CFBundleExecutable</key><string>$(BINARY)</string>' \
	  '  <key>LSMinimumSystemVersion</key><string>11.0</string>' \
	  '  <key>LSUIElement</key><true/>' \
	  '</dict></plist>' > $(APP)/Contents/Info.plist
	printf 'APPL????' > $(APP)/Contents/PkgInfo
	@echo "built $(APP) — open it, or: open $(APP) --args menubar"

install: build
	install -d $(PREFIX)/bin
	install -m 0755 $(BINARY) $(PREFIX)/bin/$(BINARY)

uninstall:
	rm -f $(PREFIX)/bin/$(BINARY)

clean:
	rm -rf $(BINARY) $(APP)
