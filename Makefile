# OmniVM Makefile (Linux / Cloud Hypervisor only)
# Build ovmd, ovm CLI, guest harness, and MCP servers.

SHELL := /bin/bash

# Go settings
GO := go
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
VERSION_PKG := github.com/SalyyS1/omnivm/internal/version
LDFLAGS := -X $(VERSION_PKG).version=$(VERSION)
GOFLAGS := -trimpath -ldflags "$(LDFLAGS)"

# Output directory
BIN_DIR := bin

# Host platform
HOST_OS := $(shell uname -s | tr A-Z a-z)
HOST_ARCH := $(shell uname -m)
ifeq ($(HOST_ARCH),aarch64)
	HOST_ARCH := arm64
endif
ifeq ($(HOST_ARCH),x86_64)
	HOST_ARCH := amd64
endif

# Guest binaries are static Linux binaries matching the host arch
# (Cloud Hypervisor runs guests natively).
GUEST_OS := linux
GUEST_ARCH := $(HOST_ARCH)

# Pinned Cloud Hypervisor release and guest kernel release
CH_VERSION := v43.0
CH_KERNEL_RELEASE := ch-release-v6.16.9-20251112

ALL_TARGETS := ovmd ovm harness mcp mcp-guest

.PHONY: all ovmd ovm harness mcp mcp-guest clean test test-unit integration test-linux cloud-hypervisor kernel kernel-build deb release-linux-tarball

all: $(ALL_TARGETS)

# ovmd — the host daemon
ovmd:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/ovmd ./cmd/ovmd

# ovm — the CLI
ovm:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/ovm ./cmd/ovm

# ovm-harness — guest PID 1 (static Linux binary)
harness:
	@mkdir -p $(BIN_DIR)
	GOOS=$(GUEST_OS) GOARCH=$(GUEST_ARCH) CGO_ENABLED=0 \
		$(GO) build $(GOFLAGS) -o $(BIN_DIR)/ovm-harness ./cmd/ovm-harness

# ovm-mcp — MCP server for LLM integration (host-side)
mcp:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/ovm-mcp ./cmd/ovm-mcp

# ovm-mcp-guest — MCP server for programs inside VMs (guest-side)
mcp-guest:
	@mkdir -p $(BIN_DIR)
	GOOS=$(GUEST_OS) GOARCH=$(GUEST_ARCH) CGO_ENABLED=0 \
		$(GO) build $(GOFLAGS) -o $(BIN_DIR)/ovm-mcp-guest ./cmd/ovm-mcp-guest

# Download Cloud Hypervisor static binary
cloud-hypervisor:
	@mkdir -p $(BIN_DIR)
	@echo "==> Downloading Cloud Hypervisor $(CH_VERSION) ($(HOST_ARCH))..."
ifeq ($(HOST_ARCH),amd64)
	curl -fsSL -o $(BIN_DIR)/cloud-hypervisor \
		"https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/$(CH_VERSION)/cloud-hypervisor-static"
	curl -fsSL -o $(BIN_DIR)/ch-remote \
		"https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/$(CH_VERSION)/ch-remote-static"
else ifeq ($(HOST_ARCH),arm64)
	curl -fsSL -o $(BIN_DIR)/cloud-hypervisor \
		"https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/$(CH_VERSION)/cloud-hypervisor-static-aarch64"
	curl -fsSL -o $(BIN_DIR)/ch-remote \
		"https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/$(CH_VERSION)/ch-remote-static-aarch64"
endif
	chmod +x $(BIN_DIR)/cloud-hypervisor $(BIN_DIR)/ch-remote
	@echo "==> Cloud Hypervisor $(CH_VERSION) installed to $(BIN_DIR)/"

# Download prebuilt microVM kernel from Cloud Hypervisor's linux repo.
# The CH project maintains kernels with virtiofs, vsock, and virtio-net built-in
# for x86_64 and arm64. Use `make kernel-build` to compile from source instead.
kernel:
	@mkdir -p $(HOME)/.omnivm/kernel
	@if [ -f $(HOME)/.omnivm/kernel/vmlinux ]; then \
		echo "Kernel already exists at $(HOME)/.omnivm/kernel/vmlinux"; \
		echo "Delete it and re-run to re-download"; \
	else \
		echo "==> Downloading Cloud Hypervisor kernel $(CH_KERNEL_RELEASE) ($(HOST_ARCH))..." && \
		case $(HOST_ARCH) in \
			amd64) \
				curl -fsSL -o $(HOME)/.omnivm/kernel/vmlinux \
					"https://github.com/cloud-hypervisor/linux/releases/download/$(CH_KERNEL_RELEASE)/vmlinux-x86_64" ;; \
			arm64) \
				curl -fsSL -o $(HOME)/.omnivm/kernel/vmlinux \
					"https://github.com/cloud-hypervisor/linux/releases/download/$(CH_KERNEL_RELEASE)/Image-arm64" ;; \
			*) \
				echo "No prebuilt kernel for $(HOST_ARCH). Use 'make kernel-build' to compile from source."; \
				exit 1 ;; \
		esac && \
		echo "==> Kernel installed to $(HOME)/.omnivm/kernel/vmlinux"; \
	fi

# Build microVM kernel from source (fallback for unsupported archs).
# Uses Cloud Hypervisor's kernel branch with ch_defconfig (virtiofs + vsock built-in).
# Requires: build-essential flex bison libelf-dev libssl-dev bc
kernel-build:
	@mkdir -p $(HOME)/.omnivm/kernel
	@if [ -f $(HOME)/.omnivm/kernel/vmlinux ]; then \
		echo "Kernel already exists at $(HOME)/.omnivm/kernel/vmlinux"; \
		echo "Delete it and re-run to rebuild"; \
	else \
		TMPDIR=$$(mktemp -d) && \
		echo "==> Cloning Cloud Hypervisor kernel ($(CH_KERNEL_RELEASE))..." && \
		git clone --depth 1 --branch $(CH_KERNEL_RELEASE) \
			https://github.com/cloud-hypervisor/linux.git $$TMPDIR && \
		echo "==> Configuring (ch_defconfig)..." && \
		$(MAKE) -C $$TMPDIR ch_defconfig && \
		echo "==> Building vmlinux (this may take ~10 minutes)..." && \
		$(MAKE) -C $$TMPDIR -j$$(nproc) vmlinux && \
		cp $$TMPDIR/vmlinux $(HOME)/.omnivm/kernel/vmlinux && \
		rm -rf $$TMPDIR && \
		echo "==> Kernel installed to $(HOME)/.omnivm/kernel/vmlinux"; \
	fi

# Run unit tests
test:
	$(GO) test ./...

# Run unit tests only (internal packages)
test-unit:
	$(GO) test ./internal/...

# Run integration tests (requires KVM, root, built binaries, kernel, cloud-hypervisor, virtiofsd)
# Use SHORT=1 to skip slow pause/snapshot tests.
integration: all
ifdef SHORT
	$(GO) test -tags integration -v -count=1 -short -timeout 15m ./test/integration/
else
	$(GO) test -tags integration -v -count=1 -timeout 15m ./test/integration/
endif

# Run only the Cloud Hypervisor backend tests
test-linux: all
ifdef SHORT
	$(GO) test -tags integration -v -count=1 -short -timeout 15m -run 'TestLinux_' ./test/integration/
else
	$(GO) test -tags integration -v -count=1 -timeout 15m -run 'TestLinux_' ./test/integration/
endif

# Linux .deb package
# Requires: cloud-hypervisor downloaded (make cloud-hypervisor) and kernel (make kernel).
deb: $(ALL_TARGETS)
	$(MAKE) -C debian omnivm \
		VERSION=$(VERSION) ARCH=$(HOST_ARCH) BIN_DIR=../$(BIN_DIR)

# Linux release tarball (alternative to .deb for non-apt users)
release-linux-tarball: $(ALL_TARGETS)
	@mkdir -p /tmp/omnivm-tarball-staging/{bin,lib,share/kernel}
	cp $(BIN_DIR)/ovm $(BIN_DIR)/ovmd $(BIN_DIR)/ovm-mcp \
		/tmp/omnivm-tarball-staging/bin/
	cp $(BIN_DIR)/ovm-harness $(BIN_DIR)/ovm-mcp-guest \
		$(BIN_DIR)/cloud-hypervisor $(BIN_DIR)/ch-remote \
		/tmp/omnivm-tarball-staging/lib/
	cp $(HOME)/.omnivm/kernel/vmlinux /tmp/omnivm-tarball-staging/share/kernel/
	tar czf omnivm-$(VERSION)-linux-$(HOST_ARCH).tar.gz \
		-C /tmp/omnivm-tarball-staging bin lib share
	@rm -rf /tmp/omnivm-tarball-staging
	@echo "==> Built omnivm-$(VERSION)-linux-$(HOST_ARCH).tar.gz"

# Clean build artifacts
clean:
	rm -rf $(BIN_DIR)
