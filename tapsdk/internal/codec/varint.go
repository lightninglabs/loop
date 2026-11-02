package codec

import (
	"encoding/binary"
	"fmt"
	"io"
)

// WriteVarInt writes val using the compact encoding defined by BIP-0174/LND TLV
// helpers.
func WriteVarInt(w io.Writer, val uint64, scratch []byte) error {
	if len(scratch) < 9 {
		return fmt.Errorf("scratch buffer too small")
	}

	switch {
	case val < 0xfd:
		scratch[0] = uint8(val)
		_, err := w.Write(scratch[:1])
		return err

	case val <= 0xffff:
		scratch[0] = 0xfd
		binary.BigEndian.PutUint16(scratch[1:3], uint16(val))
		_, err := w.Write(scratch[:3])
		return err

	case val <= 0xffffffff:
		scratch[0] = 0xfe
		binary.BigEndian.PutUint32(scratch[1:5], uint32(val))
		_, err := w.Write(scratch[:5])
		return err

	default:
		scratch[0] = 0xff
		binary.BigEndian.PutUint64(scratch[1:9], val)
		_, err := w.Write(scratch[:9])
		return err
	}
}

// ReadVarInt reads a compactly encoded integer as defined by BIP-0174/LND TLV
// helpers.
func ReadVarInt(r io.Reader, scratch []byte) (uint64, error) {
	if len(scratch) < 8 {
		return 0, fmt.Errorf("scratch buffer too small")
	}

	_, err := io.ReadFull(r, scratch[:1])
	if err != nil {
		return 0, err
	}

	disc := scratch[0]
	switch {
	case disc < 0xfd:
		return uint64(disc), nil

	case disc == 0xfd:
		_, err := io.ReadFull(r, scratch[:2])
		if err != nil {
			return 0, err
		}
		return uint64(binary.BigEndian.Uint16(scratch[:2])), nil

	case disc == 0xfe:
		_, err := io.ReadFull(r, scratch[:4])
		if err != nil {
			return 0, err
		}
		return uint64(binary.BigEndian.Uint32(scratch[:4])), nil

	default:
		_, err := io.ReadFull(r, scratch[:8])
		if err != nil {
			return 0, err
		}
		return binary.BigEndian.Uint64(scratch[:8]), nil
	}
}
