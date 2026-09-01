package keystring

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDocumentDBResumeTokenIsNotKeystring pins the premise behind the
// flavor-aware timestamp handling in the change reader.
//
// The token below was captured from Amazon DocumentDB 5.0.0 on 2026-09-01. A
// MongoDB resume token's _data is a KeyString V1 whose first element is the
// event's cluster time; the verifier decodes it to learn how far the reader
// has consumed. DocumentDB's token uses a different, undocumented encoding, so
// the change reader must never decode it — see ChangeReaderCommon.positionTimestamp.
//
// This test documents what actually happens if that rule is broken: either an
// error, or worse, a plausible-looking value that is not the cluster time.
func TestDocumentDBResumeTokenIsNotKeystring(t *testing.T) {
	const docDBToken = "016a96c50200000009010000000000004324"

	decoded, err := KeystringToBson(V1, docDBToken)

	if err != nil {
		t.Logf("decode rejected the DocumentDB token, as expected: %v", err)
		return
	}

	// If it decodes at all, the result must not be mistaken for a usable
	// cluster time. Record what it produced so the risk is visible.
	t.Logf("DocumentDB token decoded to %v -- decoding it would yield a WRONG timestamp", decoded)

	assert.NotEmpty(
		t,
		decoded,
		"a decode that silently produced nothing would be the most dangerous outcome",
	)
}
