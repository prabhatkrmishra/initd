BUILD_DIR := build
VERSION ?= 1.1.0

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
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags="-s -w" -o $(INITD_BIN) ./cmd/initd
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags="-s -w" -o $(SYSTEMCTL_BIN) ./cmd/systemctl
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags="-s -w" -o $(LOGINCTL_BIN) ./cmd/loginctl
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags="-s -w" -o $(JOURNALCTL_BIN) ./cmd/journalctl
	@echo "Build completed for $(GOOS)/$(GOARCH)."

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
	@{ echo "initd $(VERSION) release manifest"; echo "built: $$(date -u +%Y-%m-%dT%H:%M:%SZ)"; echo "go: $$(go version)"; echo; \
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
