BUILD_DIR := build
VERSION ?= 1.0.0

GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

OUTPUT_DIR := $(BUILD_DIR)/$(GOOS)-$(GOARCH)

INITD_BIN := $(OUTPUT_DIR)/initd
SYSTEMCTL_BIN := $(OUTPUT_DIR)/systemctl

.PHONY: build build-all package clean

build:
	@mkdir -p $(OUTPUT_DIR)
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags="-s -w" -o $(INITD_BIN) ./cmd/initd
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags="-s -w" -o $(SYSTEMCTL_BIN) ./cmd/systemctl
	@echo "Build completed for $(GOOS)/$(GOARCH)."

build-all:
	@$(MAKE) build GOOS=linux GOARCH=amd64
	@$(MAKE) build GOOS=linux GOARCH=arm64
	@echo "All architectures built."

package: build-all
	@mkdir -p releases
	@for ARCH in amd64 arm64; do \
		cd "$(BUILD_DIR)/linux-$$ARCH" && zip -q "../../releases/initd_$(VERSION)_linux_$${ARCH}.zip" initd systemctl && cd - > /dev/null; \
		echo "Created releases/initd_$(VERSION)_linux_$${ARCH}.zip"; \
	done
	@sha256sum releases/*.zip 2>/dev/null || true

clean:
	rm -rf $(BUILD_DIR)/*
	@echo "Clean completed."
