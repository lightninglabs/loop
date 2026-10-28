package fsm

import "encoding/json/v2"

// JSONCodec returns a strict JSON codec. Unknown members and duplicate object
// names are rejected rather than silently accepting incompatible checkpoints.
// Business structs should use concrete fields and still validate all security
// invariants: successful decoding does not authorize a financial operation.
func JSONCodec[T any]() Codec[T] {
	return Codec[T]{
		Encode: func(value T) ([]byte, error) {
			return json.Marshal(value)
		},
		Decode: func(encoded []byte) (T, error) {
			var value T
			err := json.Unmarshal(encoded, &value,
				json.RejectUnknownMembers(true))
			return value, err
		},
	}
}
