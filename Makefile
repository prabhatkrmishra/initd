BUILD_DIR := build
VERSION ?= 1.1.0

# Stamp every binary with the source it came from. Without this a rebuild of a
# dirty tree and a months-old install both report the same version, and there
# is no way to tell a patched daemon from the one that broke.
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
GIT_DIRTY := $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo -dirty)
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
STAMP := $(GIT_COMMIT)$(GIT_DIRTY)
LDFLAGS := -s -w -X initd/internal/build.Version=$(VERSION) -X initd/internal/build.Meta=$(STAMP) -X initd/internal/build.BuildDate=$(BUILD_DATE)
GO_FLAGS := -ldflags="$(LDFLAGS)"

GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

OUTPUT_DIR := $(BUILD_DIR)/$(GOOS)-$(GOARCH)

INITD_BIN := $(OUTPUT_DIR)/initd
SYSTEMCTL_BIN := $(OUTPUT_DIR)/systemctl
LOGINCTL_BIN := $(OUTPUT_DIR)/loginctl
JOURNALCTL_BIN := $(OUTPUT_DIR)/journalctl

.PHONY: build build-all package clean

build:
	@mkdir -p $(OUTPUT_DIR)
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GO_FLAGS) -o $(INITD_BIN) ./cmd/initd
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GO_FLAGS) -o $(SYSTEMCTL_BIN) ./cmd/systemctl
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GO_FLAGS) -o $(LOGINCTL_BIN) ./cmd/loginctl
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GO_FLAGS) -o $(JOURNALCTL_BIN) ./cmd/journalctl
	@echo "Build completed for $(GOOS)/$(GOARCH) (version $(VERSION)+$(STAMP))."

build-all:
	@$(MAKE) build GOOS=linux GOARCH=amd64
	@$(MAKE) build GOOS=linux GOARCH=arm64
	@echo "All architectures built."

package: build-all
	@mkdir -p releases
	@command -v zip >/dev/null || { echo "zip not found; install zip to package releases" >&2; exit 1; }
	@for ARCH in amd64 arm64; do \
		zip -q -j "releases/initd_$(VERSION)_linux_$${ARCH}.zip" \
			"$(BUILD_DIR)/linux-$$ARCH/initd" \
			"$(BUILD_DIR)/linux-$$ARCH/systemctl" \
			"$(BUILD_DIR)/linux-$$ARCH/loginctl" \
			"$(BUILD_DIR)/linux-$$ARCH/journalctl" \
			install.sh && \
		echo "Created releases/initd_$(VERSION)_linux_$${ARCH}.zip (initd, systemctl, loginctl, journalctl, install.sh)"; \
	done
	@sha256sum releases/initd_$(VERSION)_linux_*.zip > releases/initd_$(VERSION)_SHA256SUMS
	@{ echo "initd $(VERSION) release manifest"; echo "source: $(GIT_COMMIT)$(GIT_DIRTY)"; echo "built: $$(date -u +%Y-%m-%dT%H:%M:%SZ)"; echo "go: $$(go version)"; echo; \
	  for ARCH in amd64 arm64; do echo "linux/$$ARCH:"; \
	    for BIN in initd systemctl loginctl journalctl; do \
	      f="$(BUILD_DIR)/linux-$$ARCH/$$BIN"; [ -x "$$f" ] || { echo "missing $$f" >&2; exit 1; }; \
	      h="$$(sha256sum "$$f" | cut -d' ' -f1)" && [ -n "$$h" ] || exit 1; printf "  %s %s\n" "$$h" "$$BIN"; \
	    done; \
	    z="releases/initd_$(VERSION)_linux_$$ARCH.zip"; \
	    zh="$$(sha256sum "$$z" | cut -d' ' -f1)" && [ -n "$$zh" ] || exit 1; printf "  %s %s\n" "$$zh" "$$z"; \
	  done; } > releases/initd_$(VERSION)_MANIFEST.txt
	@echo "Wrote releases/initd_$(VERSION)_SHA256SUMS and releases/initd_$(VERSION)_MANIFEST.txt"
	@cat releases/initd_$(VERSION)_MANIFEST.txt

clean:
	rm -rf $(BUILD_DIR)/*
	@echo "Clean completed."
