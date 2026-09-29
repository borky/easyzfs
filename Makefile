# EasyZFS — build: front (web/) → dist/ → binario estático CGO_ENABLED=0
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILD   ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
# Fork default: a build from this checkout is updated from this checkout, never
# from upstream's releases (see main.updateChannel). UPDATE_CHANNEL=github
# restores upstream's in-app updater.
UPDATE_CHANNEL ?= local
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.build=$(BUILD) -X main.updateChannel=$(UPDATE_CHANNEL)

.PHONY: build web go clean install update not-root

build: web go

# Front: si existe web/ se compila; si no, se usa el dist/ placeholder.
web:
	@if [ -d web ]; then \
		cd web && npm ci && npm run build && \
		cd .. && rm -rf dist && cp -r web/dist dist ; \
	else \
		echo "web/ does not exist: using a placeholder dist/" ; \
	fi

go:
	@if [ ! -d dist ]; then \
		if [ -d web/dist ]; then cp -r web/dist dist; \
		else echo "dist/ does not exist: run 'make web' first"; exit 1; fi \
	fi
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o easyzfs .

clean:
	rm -f easyzfs

# Install from this checkout. Builds as the invoking user, then runs only the
# install step as root, so sudo never builds in (and leaves root-owned files
# in) the checkout. Extra installer flags: make install INSTALL_ARGS="--port 9090"
install: not-root build
	sudo bash deploy/install.sh --binary ./easyzfs $(INSTALL_ARGS)

# Update an install made from this checkout: git pull first, then this.
# Replaces the binary and the root helper and restarts; config, data, users,
# the unit and anything added to /etc/easyzfs/env are left alone.
update: not-root build
	sudo bash deploy/install.sh --update --binary ./easyzfs

# 'sudo make install' would build as root and leave root-owned node_modules/,
# dist/ and easyzfs in the checkout, breaking the next build as the user.
not-root:
	@if [ "$$(id -u)" = "0" ]; then echo "Run 'make install' / 'make update' as your user, without sudo: it asks for sudo only to install." >&2; exit 1; fi
