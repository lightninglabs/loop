.DEFAULT_GOAL := build

PKG := github.com/lightninglabs/loop

GOTEST := GO111MODULE=on go test -v

GO_BIN := ${GOPATH}/bin
GOBUILD := GO111MODULE=on go build -v
GOINSTALL := GO111MODULE=on go install -v
GOMOD := GO111MODULE=on go mod

COMMIT := $(shell git describe --abbrev=40 --dirty)
LDFLAGS := -ldflags "-X $(PKG)/build.Commit=$(COMMIT)"

GOFILES_NOVENDOR = $(shell find . -type f -name '*.go' -not -path "./vendor/*")
GOLIST := go list $(PKG)/... | grep -v '/vendor/'

LINT_BIN := $(GO_BIN)/golangci-lint
LINT_PKG := github.com/golangci/golangci-lint/cmd/golangci-lint
LINT_COMMIT := v1.18.0
LINT = $(LINT_BIN) run -v

DEPGET := cd /tmp && GO111MODULE=on go get -v
XARGS := xargs -L 1

TEST_FLAGS = -test.timeout=20m

UNIT := $(GOLIST) | $(XARGS) env $(GOTEST) $(TEST_FLAGS)

GREEN := "\\033[0;32m"
NC := "\\033[0m"
define print
	echo $(GREEN)$1$(NC)
endef

$(LINT_BIN):
	@$(call print, "Fetching linter")
	$(DEPGET) $(LINT_PKG)@$(LINT_COMMIT)

unit: 
	@$(call print, "Running unit tests.")
	$(UNIT)

fmt:
	@$(call print, "Formatting source.")
	gofmt -l -w -s $(GOFILES_NOVENDOR)

lint: $(LINT_BIN)
	@$(call print, "Linting source.")
	$(LINT)

mod-tidy:
	@$(call print, "Tidying modules.")
	$(GOMOD) tidy

mod-check:
	@$(call print, "Checking modules.")
	$(GOMOD) tidy
	if test -n "$$(git status | grep -e "go.mod\|go.sum")"; then echo "Running go mod tidy changes go.mod/go.sum"; git status; git diff; exit 1; fi

# ============
# INSTALLATION
# ============

build:
	@$(call print, "Building debug loop and loopd.")
	$(GOBUILD) -o loop-debug $(LDFLAGS) $(PKG)/cmd/loop
	$(GOBUILD) -o loopd-debug $(LDFLAGS) $(PKG)/cmd/loopd

install:
	@$(call print, "Installing loop and loopd.")
	$(GOINSTALL) $(LDFLAGS) $(PKG)/cmd/loop
	$(GOINSTALL) $(LDFLAGS) $(PKG)/cmd/loopd
