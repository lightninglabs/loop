.DEFAULT_GOAL := build

PKG := github.com/lightninglabs/loop
TOOLS_DIR := tools

GOTEST := GO111MODULE=on go test -v


GOIMPORTS_PKG := github.com/rinchsan/gosimports/cmd/gosimports

GO_BIN := ${GOPATH}/bin
GOIMPORTS_BIN := $(GO_BIN)/gosimports

GOBUILD := CGO_ENABLED=0 GO111MODULE=on go build -v
GOINSTALL := CGO_ENABLED=0 GO111MODULE=on go install -v
GOMOD := GO111MODULE=on go mod

COMMIT := $(shell git describe --abbrev=40 --dirty --tags)
COMMIT_HASH := $(shell git rev-parse HEAD)
DIRTY := $(shell git diff-index --quiet HEAD -- || echo dirty)
LDFLAGS := -ldflags "-X $(PKG).Commit=$(COMMIT) -X $(PKG).CommitHash=$(COMMIT_HASH) -X $(PKG).Dirty=$(DIRTY)"
DEV_TAGS = dev

GOFILES_NOVENDOR = $(shell find . -type f -name '*.go' -not -path "./vendor/*" -not -name "*pb.go" -not -name "*pb.gw.go" -not -name "*.pb.json.go")
GOLIST := go list $(PKG)/... | grep -v '/vendor/'

XARGS := xargs -L 1

TEST_FLAGS = -test.timeout=20m

UNIT := $(GOLIST) | $(XARGS) env $(GOTEST) $(TEST_FLAGS)

# Linting uses a lot of memory, so keep it under control by limiting the number
# of workers if requested.
ifneq ($(workers),)
LINT_WORKERS = --concurrency=$(workers)
endif

DOCKER_TOOLS = docker run \
  --rm \
  -v $(shell bash -c "go env GOCACHE 2>/dev/null || (mkdir -p /tmp/go-cache; echo /tmp/go-cache)"):/tmp/build/.cache \
  -v $(shell bash -c "go env GOMODCACHE 2>/dev/null || (mkdir -p /tmp/go-modcache; echo /tmp/go-modcache)"):/tmp/build/.modcache \
  -v $$(pwd):/build loop-tools

DOCKER_RELEASE_BUILDER = docker run \
  --rm \
  -v $(shell bash -c "go env GOCACHE 2>/dev/null || (mkdir -p /tmp/go-cache; echo /tmp/go-cache)"):/tmp/build/.cache \
  -v $(shell bash -c "go env GOMODCACHE 2>/dev/null || (mkdir -p /tmp/go-modcache; echo /tmp/go-modcache)"):/tmp/build/.modcache \
  -v $$(pwd):/repo \
  -e LOOPBUILDSYS='$(buildsys)' \
  loop-release-builder

GREEN=\033[0;32m
NC=\033[0m
define print
	@printf '%b%s%b\n' '${GREEN}' $1 '${NC}'
endef

# ============
# DEPENDENCIES
# ============

$(GOIMPORTS_BIN):
	@$(call print, "Installing goimports.")
	cd $(TOOLS_DIR); go install -trimpath $(GOIMPORTS_PKG)


# ============
# INSTALLATION
# ============

build:
	@$(call print, "Building debug loop and loopd.")
	$(GOBUILD) -tags="$(DEV_TAGS)" -o loop-debug $(LDFLAGS) $(PKG)/cmd/loop
	$(GOBUILD) -tags="$(DEV_TAGS)" -o loopd-debug $(LDFLAGS) $(PKG)/cmd/loopd

install:
	@$(call print, "Installing loop and loopd.")
	$(GOINSTALL) -tags="${tags}" $(LDFLAGS) $(PKG)/cmd/loop
	$(GOINSTALL) -tags="${tags}" $(LDFLAGS) $(PKG)/cmd/loopd

# docker-release: Same as release.sh but within a docker container to support
# reproducible builds on any platform.
docker-release: docker-release-builder
	@$(call print, "Building release binaries in docker.")
	@if [ "$(tag)" = "" ]; then echo "Must specify tag=<commit_or_tag>!"; exit 1; fi
	$(DOCKER_RELEASE_BUILDER) bash release.sh $(tag)

rpc:
	@$(call print, "Compiling protos.")
	cd ./swapserverrpc; ./gen_protos_docker.sh
	cd ./looprpc; ./gen_protos_docker.sh

rpc-check: rpc
	@$(call print, "Verifying protos.")
	if test -n "$$(git status --porcelain)"; then echo "Protos not properly formatted or not compiled with correct version!"; git status; git diff; exit 1; fi

rpc-js-compile:
	@$(call print, "Compiling JSON/WASM stubs.")
	GOOS=js GOARCH=wasm $(GOBUILD) $(PKG)/looprpc

rpc-format:
	cd ./looprpc; find . -name "*.proto" | xargs clang-format --style=file -i
	cd ./swapserverrpc; find . -name "*.proto" | xargs clang-format --style=file -i

clean:
	@$(call print, "Cleaning up.")
	rm -f ./loop-debug ./loopd-debug
	rm -rf ./vendor

# =======
# TESTING
# =======

unit:
	@$(call print, "Running unit tests.")
	$(UNIT)

unit-race:
	@$(call print, "Running unit race tests.")
	$(UNIT) -race

unit-postgres:
	@$(call print, "Running unit tests with postgres.")
	$(UNIT) -tags=test_db_postgres

unit-postgres-race:
	@$(call print, "Running unit race tests with postgres.")
	$(UNIT) -race -tags=test_db_postgres

# =========
# UTILITIES
# =========

fmt: $(GOIMPORTS_BIN)
	@$(call print, "Fixing imports.")
	gosimports -w $(GOFILES_NOVENDOR)
	@$(call print, "Formatting source.")
	gofmt -l -w -s $(GOFILES_NOVENDOR)

lint: docker-tools
	@$(call print, "Linting source.")
	$(DOCKER_TOOLS) golangci-lint run -v $(LINT_WORKERS)

docker-tools:
	@$(call print, "Building tools docker image.")
	docker build -q -t loop-tools $(TOOLS_DIR)

docker-release-builder:
	@$(call print, "Building release builder docker image.")
	docker build -q -t loop-release-builder -f release.Dockerfile .

mod-tidy:
	@$(call print, "Tidying modules.")
	$(GOMOD) tidy

mod-check:
	@$(call print, "Checking modules.")
	GOPROXY=direct $(GOMOD) tidy
	cd swapserverrpc/ && GOPROXY=direct $(GOMOD) tidy
	cd looprpc/ && GOPROXY=direct $(GOMOD) tidy
	cd tools/ && GOPROXY=direct $(GOMOD) tidy
	if test -n "$$(git status --porcelain)"; then echo "Running go mod tidy changes go.mod/go.sum"; git status; git diff; exit 1; fi

sqlc:
	@$(call print, "Generating sql models and queries in Go")
	./scripts/gen_sqlc_docker.sh

sqlc-check: sqlc
	@$(call print, "Verifying sql code generation.")
	if test -n "$$(git status --porcelain '*.go')"; then echo "SQL models not properly generated!"; git status --porcelain '*.go'; exit 1; fi

fsm:
	@$(call print, "Generating state machine docs")
	./scripts/fsm-generate.sh;
.PHONY: fsm
