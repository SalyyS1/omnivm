# debian/Makefile — Build .deb packages for OmniVM
#
# Called from the top-level Makefile:
#   make deb            → omnivm_$(VERSION)_$(ARCH).deb

SHELL := /bin/bash

# Inherited from parent Makefile
VERSION ?= dev
ARCH ?= amd64
BIN_DIR ?= ../bin
KERNEL_PATH ?= $(HOME)/.omnivm/kernel/vmlinux

# Debian version: strip leading 'v' (dpkg requires version starts with a digit)
DEB_VERSION := $(patsubst v%,%,$(VERSION))

# Staging directories
STAGE_CORE := _build/omnivm

# Output
OUT_CORE := omnivm_$(DEB_VERSION)_$(ARCH).deb

.PHONY: omnivm clean

omnivm: $(OUT_CORE)

$(OUT_CORE):
	@echo "==> Building $(OUT_CORE)"
	rm -rf $(STAGE_CORE)
	# DEBIAN metadata
	mkdir -p $(STAGE_CORE)/DEBIAN
	sed -e 's/__VERSION__/$(DEB_VERSION)/g' -e 's/__ARCH__/$(ARCH)/g' \
		omnivm/DEBIAN/control > $(STAGE_CORE)/DEBIAN/control
	cp omnivm/DEBIAN/postinst $(STAGE_CORE)/DEBIAN/postinst
	chmod 755 $(STAGE_CORE)/DEBIAN/postinst
	# /usr/bin/ — user-facing binaries
	mkdir -p $(STAGE_CORE)/usr/bin
	cp $(BIN_DIR)/ovm $(STAGE_CORE)/usr/bin/
	cp $(BIN_DIR)/ovmd $(STAGE_CORE)/usr/bin/
	cp $(BIN_DIR)/ovm-mcp $(STAGE_CORE)/usr/bin/
	# /usr/lib/omnivm/ — internal binaries
	mkdir -p $(STAGE_CORE)/usr/lib/omnivm
	cp $(BIN_DIR)/ovm-harness $(STAGE_CORE)/usr/lib/omnivm/
	cp $(BIN_DIR)/ovm-mcp-guest $(STAGE_CORE)/usr/lib/omnivm/
	cp $(BIN_DIR)/cloud-hypervisor $(STAGE_CORE)/usr/lib/omnivm/
	cp $(BIN_DIR)/ch-remote $(STAGE_CORE)/usr/lib/omnivm/
	# /usr/share/omnivm/kernel/ — microVM kernel
	mkdir -p $(STAGE_CORE)/usr/share/omnivm/kernel
	cp $(KERNEL_PATH) $(STAGE_CORE)/usr/share/omnivm/kernel/vmlinux
	# /etc/omnivm/ — reserved for future config
	mkdir -p $(STAGE_CORE)/etc/omnivm
	# Build .deb
	dpkg-deb --build --root-owner-group $(STAGE_CORE) $(OUT_CORE)
	@echo "==> Built $(OUT_CORE)"


clean:
	rm -rf _build *.deb
