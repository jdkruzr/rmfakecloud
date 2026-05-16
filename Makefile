VERSION :=$(shell git describe --tags --always)
LDFLAGS := "-s -w -X main.version=$(VERSION)"
OUT_DIR := dist
CMD := ./cmd/rmfakecloud
BINARY := rmfakecloud
BUILD = go build -ldflags $(LDFLAGS) -o $(@) $(CMD) 
ASSETS = ui/dist
GOFILES := $(shell find . -iname '*.go' ! -iname "*_test.go")
GOFILES += $(ASSETS)
UIFILES := $(shell find ui/src)
UIFILES += $(shell find ui/public)
UIFILES += ui/package.json
TARGETS := $(addprefix $(OUT_DIR)/$(BINARY)-, x64 armv6 armv7 arm64 win64 docker)
PNPM	= cd ui; pnpm

.PHONY: all run runui clean test testgo testui swagger swagger-install swagger-check

SWAG ?= $(shell command -v swag 2>/dev/null || echo $(shell go env GOPATH)/bin/swag)

build: $(OUT_DIR)/$(BINARY)-x64

all: $(TARGETS)

$(OUT_DIR)/$(BINARY)-x64:$(GOFILES)
	GOOS=linux $(BUILD)

$(OUT_DIR)/$(BINARY)-armv6:$(GOFILES)
	GOARCH=arm GOARM=6 $(BUILD)

$(OUT_DIR)/$(BINARY)-armv7:$(GOFILES)
	GOARCH=arm GOARM=7 $(BUILD)

$(OUT_DIR)/$(BINARY)-win64:$(GOFILES)
	GOOS=windows $(BUILD)

$(OUT_DIR)/$(BINARY)-arm64:$(GOFILES)
	GOARCH=arm64 $(BUILD)

$(OUT_DIR)/$(BINARY)-docker:$(GOFILES)
	CGO_ENABLED=0 $(BUILD)

container: $(OUT_DIR)/$(BINARY)-docker
	docker build -t rmfakecloud -f Dockerfile.make .
	
run: $(ASSETS)
	go run $(CMD) $(ARG)

$(ASSETS): $(UIFILES) ui/pnpm-lock.yaml
	#@cp ui/node_modules/pdfjs-dist/build/pdf.worker.js ui/public/
	$(PNPM) build
	@#remove unneeded stuff, todo: eject
	@rm ui/build/service-worker.js ui/build/precache-manifest* ui/build/asset-manifest.json 2> /dev/null || true

ui/pnpm-lock.yaml: ui/node_modules ui/package.json
	$(PNPM) i
	@touch -mr $(shell ls -Atd $? | head -1) $@

ui/node_modules:
	mkdir -p $@

runui: ui/pnpm-lock.yaml
	$(PNPM) run dev

clean:
	rm -f $(OUT_DIR)/*
	rm -fr $(ASSETS)

test: testui testgo

testui:
	echo "TODO: fix this"
	#CI=true $(PNPM) test

testgo:
	go test ./...

swagger-install:
	go install github.com/swaggo/swag/cmd/swag@latest

# Regenerates internal/ui/docs from swag annotations on the /ui/api handlers.
# Spec is checked in so it can be served by the binary and reviewed in PRs.
swagger:
	$(SWAG) init -g internal/ui/routes.go -o internal/ui/docs --parseDependency --parseInternal

# CI gate: fail if checked-in spec is out of sync with annotations.
swagger-check: swagger
	@git diff --exit-code internal/ui/docs || \
	    (echo "OpenAPI spec out of date — run 'make swagger' and commit"; exit 1)

