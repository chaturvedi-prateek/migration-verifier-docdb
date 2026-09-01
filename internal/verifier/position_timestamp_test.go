package verifier

import (
	"testing"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func newTestReader(flavor util.Flavor) *ChangeReaderCommon {
	rc := newChangeReaderCommon(src)
	rc.logger = logger.NewDefaultLogger()
	rc.clusterInfo = util.ClusterInfo{
		VersionArray: []int{5, 0, 0},
		Topology:     util.TopologyReplset,
		Flavor:       flavor,
	}

	return &rc
}

// TestPositionTimestamp_MongoDBUsesResumeToken ensures MongoDB still decodes
// the resume token, which is an exact record of what the reader has consumed.
func TestPositionTimestamp_MongoDBUsesResumeToken(t *testing.T) {
	expected := bson.Timestamp{T: 1571788022, I: 2}

	rc := newTestReader(util.FlavorMongoDB)
	rc.resumeTokenTSExtractor = func(bson.Raw) (bson.Timestamp, error) {
		return expected, nil
	}

	// Even with a newer event recorded, the token remains authoritative on
	// MongoDB.
	rc.lastChangeEventTime.Store(option.Some(bson.Timestamp{T: 99999, I: 1}))

	actual, err := rc.positionTimestamp(bson.Raw{})
	require.NoError(t, err)
	assert.Equal(t, expected, actual)
}

// TestPositionTimestamp_MongoDBPropagatesExtractorError ensures a token that
// cannot be decoded is reported rather than masked by a fallback.
func TestPositionTimestamp_MongoDBPropagatesExtractorError(t *testing.T) {
	rc := newTestReader(util.FlavorMongoDB)
	rc.resumeTokenTSExtractor = func(bson.Raw) (bson.Timestamp, error) {
		return bson.Timestamp{}, errors.New("undecodable token")
	}
	rc.lastChangeEventTime.Store(option.Some(bson.Timestamp{T: 42, I: 1}))

	_, err := rc.positionTimestamp(bson.Raw{})

	require.Error(t, err, "MongoDB must not silently fall back to event time")
	assert.ErrorContains(t, err, "undecodable")
}

// TestPositionTimestamp_DocumentDB ensures DocumentDB never invokes the
// KeyString extractor, which cannot decode its opaque token, and instead
// prefers the last event time with the server timestamp as a fallback.
func TestPositionTimestamp_DocumentDB(t *testing.T) {
	eventTS := bson.Timestamp{T: 2000, I: 1}
	serverTS := bson.Timestamp{T: 1000, I: 1}

	extractorCalled := false
	newDocDBReader := func() *ChangeReaderCommon {
		rc := newTestReader(util.FlavorDocumentDB)
		rc.resumeTokenTSExtractor = func(bson.Raw) (bson.Timestamp, error) {
			extractorCalled = true
			return bson.Timestamp{}, errors.New("keystring decode must not run on DocumentDB")
		}

		return rc
	}

	t.Run("prefers the last change event's time", func(t *testing.T) {
		rc := newDocDBReader()
		rc.lastChangeEventTime.Store(option.Some(eventTS))
		rc.lastSeenClusterTime.Store(option.Some(serverTS))

		actual, err := rc.positionTimestamp(bson.Raw{})
		require.NoError(t, err)
		assert.Equal(t, eventTS, actual)
	})

	t.Run("falls back to the server timestamp when no event has arrived", func(t *testing.T) {
		rc := newDocDBReader()
		rc.lastSeenClusterTime.Store(option.Some(serverTS))

		actual, err := rc.positionTimestamp(bson.Raw{})
		require.NoError(t, err)
		assert.Equal(
			t,
			serverTS,
			actual,
			"an idle stream must still yield a timestamp",
		)
	})

	t.Run("errors when nothing is known yet", func(t *testing.T) {
		rc := newDocDBReader()

		_, err := rc.positionTimestamp(bson.Raw{})
		require.Error(t, err)
		assert.ErrorContains(t, err, "position timestamp")
	})

	assert.False(
		t,
		extractorCalled,
		"the resume-token extractor must never run against DocumentDB",
	)
}
