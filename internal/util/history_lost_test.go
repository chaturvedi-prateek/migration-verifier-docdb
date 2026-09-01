package util

import (
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// TestIsChangeStreamHistoryLostError guards the check that stops a
// verification from silently continuing across a gap in the change stream.
//
// A false negative here is the dangerous direction: the run would resume past
// unobserved changes and report a match it cannot justify.
func TestIsChangeStreamHistoryLostError(t *testing.T) {
	t.Run("matches the MongoDB error code", func(t *testing.T) {
		err := mongo.CommandError{
			Code:    286,
			Message: "cannot resume stream",
		}

		assert.True(t, IsChangeStreamHistoryLostError(err))
	})

	t.Run("matches a wrapped error", func(t *testing.T) {
		err := errors.Wrap(
			mongo.CommandError{Code: 286, Message: "cannot resume stream"},
			"opening change stream",
		)

		assert.True(t, IsChangeStreamHistoryLostError(err))
	})

	// Captured from DocumentDB 5.0.0 on 2026-09-01 by resuming from a token
	// whose timestamp predated the change stream retention window. DocumentDB
	// reports the mechanism -- its log is a capped collection -- rather than
	// the meaning, and shares neither code nor wording with MongoDB. Without
	// this case the gate fails open: the verifier would resume across an
	// unobserved gap and report a match it cannot justify.
	t.Run("matches DocumentDB's expired-token error", func(t *testing.T) {
		err := mongo.CommandError{
			Code:    136,
			Message: "CappedPositionLost: CollectionScan died due to position in capped collection being deleted.",
		}

		assert.True(t, IsChangeStreamHistoryLostError(err))
	})

	t.Run("does not match DocumentDB's malformed-token error", func(t *testing.T) {
		// A corrupt token is a different failure and must not be reported as
		// lost history.
		err := mongo.CommandError{
			Code:    9,
			Message: "Invalid resume token: {_data: '01deadbeef...'}",
		}

		assert.False(t, IsChangeStreamHistoryLostError(err))
	})

	t.Run("matches by message when the code is unknown", func(t *testing.T) {
		// DocumentDB's code for this is undocumented, so message matching is
		// the fallback.
		for _, msg := range []string{
			"Error: ChangeStreamHistoryLost",
			"the resume point may no longer be in the oplog",
			"Resume token was not found",
		} {
			assert.True(
				t,
				IsChangeStreamHistoryLostError(errors.New(msg)),
				"should match %#q",
				msg,
			)
		}
	})

	t.Run("does not match unrelated failures", func(t *testing.T) {
		for _, err := range []error{
			nil,
			errors.New("connection refused"),
			mongo.CommandError{Code: 11601, Message: "interrupted"},
		} {
			assert.False(t, IsChangeStreamHistoryLostError(err))
		}
	})
}
