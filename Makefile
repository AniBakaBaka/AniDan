GO ?= go
NPM ?= npm
PYTHON ?= python3
IMAGE ?= anidan:local
ANIDAN_SOURCE_URL ?= /source-code

.PHONY: build backend frontend lint frontend-check source docker help

build: frontend backend

backend: source
	SOURCE_SHA=$$($(PYTHON) -c 'import hashlib; print(hashlib.sha256(open("source/anidan-source.tar.gz","rb").read()).hexdigest())') && \
	CGO_ENABLED=0 $(GO) build -buildvcs=false -trimpath -ldflags="-s -w -X github.com/AniBakaBaka/AniDan/internal/server.SourceArchiveSHA256=$$SOURCE_SHA" -o bin/anidan ./cmd/anidan

frontend:
	cd web && $(NPM) ci --ignore-scripts --no-audit --no-fund
	cd web && VITE_ANIDAN_SOURCE_URL="$(ANIDAN_SOURCE_URL)" $(NPM) run build

lint:
	$(GO) vet ./...

# The inherited lint baseline is tracked separately from frontend compilation.
frontend-check:
	cd web && $(NPM) run check

source:
	$(PYTHON) scripts/package-source.py --output source/anidan-source.tar.gz

docker:
	docker build --build-arg ANIDAN_SOURCE_URL="$(ANIDAN_SOURCE_URL)" -t "$(IMAGE)" .

help:
	@printf '%s\n' 'make build           Build frontend, source archive and Go binary' 'make source          Package corresponding source' 'make lint            Run Go vet' 'make docker          Build image' 'docker compose up -d --build'
