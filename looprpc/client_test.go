package looprpc

import (
	"testing"

	"google.golang.org/protobuf/proto"
)

// TestInstantOutMaxSwapFeePresence verifies that an omitted fee cap remains
// distinguishable from an explicitly encoded zero while retaining the scalar
// field's original wire representation.
func TestInstantOutMaxSwapFeePresence(t *testing.T) {
	request := &InstantOutRequest{}
	if err := proto.Unmarshal(nil, request); err != nil {
		t.Fatalf("unable to unmarshal omitted cap: %v", err)
	}
	if request.GetMaxSwapFee() != nil {
		t.Fatal("omitted cap unexpectedly has presence")
	}

	// Field four, encoded as a varint with value zero. This is the same wire
	// representation used before the field gained presence semantics.
	if err := proto.Unmarshal([]byte{0x20, 0x00}, request); err != nil {
		t.Fatalf("unable to unmarshal explicit zero cap: %v", err)
	}
	if request.GetMaxSwapFee() == nil {
		t.Fatal("explicit zero cap lost presence")
	}
	if request.GetMaxSwapFeeSat() != 0 {
		t.Fatalf("expected zero cap, got %d", request.GetMaxSwapFeeSat())
	}
}
