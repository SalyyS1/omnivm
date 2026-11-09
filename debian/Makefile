# debian/Makefile — Build .deb packages for AegisVM
#
# Called from the top-level Makefile:
#   make deb            → aegisvm_$(VERSION)_$(ARCH).deb

SHELL := /bin/bash

# Inherited from parent Makefile
VERSION ?= dev
ARCH ?= amd64
BIN_DIR ?= ../bin
KERNEL_PATH ?= $(HOME)/.aegis/kernel/vmlinux

# Debian version: strip leading 'v' (dpkg requires version starts with a digit)
DEB_VERSION := $(patsubst v%,%,$(VERSION))

# Staging directories
STAGE_CORE := _build/aegisvm

# Output
OUT_CORE := aegisvm_$(DEB_VERSION)_$(ARCH).deb

.PHONY: aegisvm clean

aegisvm: $(OUT_CORE)

$(OUT_CORE):
	@echo "==> Building $(OUT_CORE)"
	rm -rf $(STAGE_CORE)
	# DEBIAN metadata
	mkdir -p $(STAGE_CORE)/DEBIAN
	sed -e 's/__VERSION__/$(DEB_VERSION)/g' -e 's/__ARCH__/$(ARCH)/g' \
		aegisvm/DEBIAN/control > $(STAGE_CORE)/DEBIAN/control
	cp aegisvm/DEBIAN/postinst $(STAGE_CORE)/DEBIAN/postinst
	chmod 755 $(STAGE_CORE)/DEBIAN/postinst
	# /usr/bin/ — user-facing binaries
	mkdir -p $(STAGE_CORE)/usr/bin
	cp $(BIN_DIR)/aegis $(STAGE_CORE)/usr/bin/
	cp $(BIN_DIR)/aegisd $(STAGE_CORE)/usr/bin/
	cp $(BIN_DIR)/aegis-mcp $(STAGE_CORE)/usr/bin/
	# /usr/lib/aegisvm/ — internal binaries
	mkdir -p $(STAGE_CORE)/usr/lib/aegisvm
	cp $(BIN_DIR)/aegis-harness $(STAGE_CORE)/usr/lib/aegisvm/
	cp $(BIN_DIR)/aegis-mcp-guest $(STAGE_CORE)/usr/lib/aegisvm/
	cp $(BIN_DIR)/cloud-hypervisor $(STAGE_CORE)/usr/lib/aegisvm/
	cp $(BIN_DIR)/ch-remote $(STAGE_CORE)/usr/lib/aegisvm/
	# /usr/share/aegisvm/kernel/ — microVM kernel
	mkdir -p $(STAGE_CORE)/usr/share/aegisvm/kernel
	cp $(KERNEL_PATH) $(STAGE_CORE)/usr/share/aegisvm/kernel/vmlinux
	# /etc/aegisvm/ — reserved for future config
	mkdir -p $(STAGE_CORE)/etc/aegisvm
	# Build .deb
	dpkg-deb --build --root-owner-group $(STAGE_CORE) $(OUT_CORE)
	@echo "==> Built $(OUT_CORE)"


clean:
	rm -rf _build *.deb
