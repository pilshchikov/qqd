VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)
BIN := qqd

INSTALL_DIR ?= $(or $(QQD_INSTALL_DIR),$(HOME)/.local/bin)
MAN_DIR ?= $(HOME)/.local/share/man/man1

.PHONY: build test vet clean install uninstall fmt release

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/qqd

test:
	go test ./...

vet:
	go vet ./...

fmt:
	go fmt ./...

clean:
	rm -f $(BIN)
	rm -rf dist

install: build
	@mkdir -p "$(INSTALL_DIR)" "$(MAN_DIR)"
	@install -m 0755 $(BIN) "$(INSTALL_DIR)/$(BIN)"
	@install -m 0644 man/qqd.1 "$(MAN_DIR)/qqd.1"
	@echo "Installed $(BIN) to $(INSTALL_DIR)/$(BIN)"
	@echo "Installed man page to $(MAN_DIR)/qqd.1"
	@case ":$$PATH:" in \
		*":$(INSTALL_DIR):"*) ;; \
		*) echo "" ; \
		   echo "Note: $(INSTALL_DIR) is not on your PATH." ; \
		   echo "      Add to your shell rc:  export PATH=\"$(INSTALL_DIR):\$$PATH\"" ;; \
	esac

uninstall:
	@rm -f "$(INSTALL_DIR)/$(BIN)" "$(MAN_DIR)/qqd.1"
	@echo "Removed $(BIN) from $(INSTALL_DIR) and man page from $(MAN_DIR)"

release: clean
	mkdir -p dist
	for os in darwin linux; do \
	  for arch in amd64 arm64; do \
	    GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
	      go build -trimpath -ldflags "$(LDFLAGS)" \
	      -o dist/qqd_$${os}_$${arch} ./cmd/qqd; \
	  done \
	done
	cd dist; \
	if command -v sha256sum >/dev/null 2>&1; then \
	  sha256sum qqd_* > checksums.txt; \
	else \
	  shasum -a 256 qqd_* > checksums.txt; \
	fi
