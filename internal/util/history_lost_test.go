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
