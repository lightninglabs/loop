PKG := github.com/lightninglabs/loop

GOTEST := GO111MODULE=on go test -v

GOLIST := go list $(PKG)/... | grep -v '/vendor/'

XARGS := xargs -L 1

TEST_FLAGS = -test.timeout=20m

UNIT := $(GOLIST) | $(XARGS) env $(GOTEST) $(TEST_FLAGS)

unit: 
	@$(call print, "Running unit tests.")
	$(UNIT)
