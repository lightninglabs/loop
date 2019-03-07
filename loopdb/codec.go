package loopdb

// itob returns an 8-byte big endian representation of v.
func itob(v uint64) []byte {
	b := make([]byte, 8)
	byteOrder.PutUint64(b, v)
	return b
}
