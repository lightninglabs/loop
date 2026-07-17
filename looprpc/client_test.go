package looprpc

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"
)

// TestStaticAddressLabelWireFields keeps local labels separate from the
// reserved funding fields of the former combined address/funding RPC.
func TestStaticAddressLabelWireFields(t *testing.T) {
	type labeledMessage interface {
		proto.Message
		GetLabel() string
	}
	for _, test := range []struct {
		name    string
		message labeledMessage
		field   byte
		retired byte
	}{
		{"request", &NewStaticAddressRequest{Label: "x"}, 0x1a, 0x12},
		{"response", &NewStaticAddressResponse{Label: "x"}, 0x22, 0x1a},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := proto.Marshal(test.message)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(encoded, []byte{test.field, 1, 'x'}) {
				t.Fatalf("unexpected label encoding: %x", encoded)
			}

			if err := proto.Unmarshal(
				[]byte{test.retired, 1, 'x'}, test.message,
			); err != nil {
				t.Fatal(err)
			}
			if test.message.GetLabel() != "" {
				t.Fatal("reserved funding field decoded as a label")
			}
		})
	}
}

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
