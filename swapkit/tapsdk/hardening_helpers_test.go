package tapsdk

import (
	"bytes"
	"testing"

	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// auditTLVRecord is one raw record of a TLV stream.
type auditTLVRecord struct {
	typ   uint64
	value []byte
}

// auditParseTLV splits a raw TLV stream into its records.
func auditParseTLV(t *testing.T, raw []byte) []auditTLVRecord {
	t.Helper()
	var (
		buf     [8]byte
		records []auditTLVRecord
	)
	r := bytes.NewReader(raw)
	for r.Len() > 0 {
		typ, err := tlv.ReadVarInt(r, &buf)
		require.NoError(t, err)
		length, err := tlv.ReadVarInt(r, &buf)
		require.NoError(t, err)
		value := make([]byte, length)
		_, err = r.Read(value)
		if length > 0 {
			require.NoError(t, err)
		}
		records = append(records, auditTLVRecord{typ: typ, value: value})
	}
	return records
}

// auditEncodeTLV serializes raw records back into a TLV stream.
func auditEncodeTLV(t *testing.T, records []auditTLVRecord) []byte {
	t.Helper()
	var (
		buf [8]byte
		out bytes.Buffer
	)
	for _, record := range records {
		require.NoError(t, tlv.WriteVarInt(&out, record.typ, &buf))
		require.NoError(t, tlv.WriteVarInt(
			&out, uint64(len(record.value)), &buf,
		))
		out.Write(record.value)
	}
	return out.Bytes()
}

// auditStripNested descends through the nested TLV records named by path and
// removes the record of type strip from the innermost stream.
func auditStripNested(t *testing.T, stream []byte, path []uint64,
	strip uint64) []byte {

	t.Helper()
	records := auditParseTLV(t, stream)
	var found bool
	if len(path) == 0 {
		kept := records[:0]
		for _, record := range records {
			if record.typ == strip {
				found = true
				continue
			}
			kept = append(kept, record)
		}
		require.True(t, found, "record %d not found", strip)
		return auditEncodeTLV(t, kept)
	}
	for i := range records {
		if records[i].typ != path[0] {
			continue
		}
		found = true
		records[i].value = auditStripNested(
			t, records[i].value, path[1:], strip,
		)
	}
	require.True(t, found, "record %d not found", path[0])
	return auditEncodeTLV(t, records)
}

// auditStripProofRecord removes one nested record from an encoded proof and
// returns the resulting single proof.
func auditStripProofRecord(t *testing.T, p *proof.Proof, path []uint64,
	strip uint64) []byte {

	t.Helper()
	var encoded bytes.Buffer
	require.NoError(t, p.Encode(&encoded))
	raw := encoded.Bytes()
	magic := bytes.Clone(raw[:proof.PrefixMagicBytesLength])
	return append(magic, auditStripNested(
		t, raw[proof.PrefixMagicBytesLength:], path, strip,
	)...)
}

// auditProofFile wraps one raw proof into a proof file.
func auditProofFile(t *testing.T, single []byte) []byte {
	t.Helper()
	file := proof.NewEmptyFile(proof.V0)
	require.NoError(t, file.AppendProofRaw(single))
	out, err := proof.EncodeFile(file)
	require.NoError(t, err)
	return out
}
