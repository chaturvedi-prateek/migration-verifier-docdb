package util

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestFlavorSelectsTimestampSource documents which server field each flavor
// reads, since the two are not interchangeable.
func TestFlavorSelectsTimestampSource(t *testing.T) {
	assert.True(
		t,
		FlavorDocumentDB.IsDocumentDB(),
		"DocumentDB must take the operationTime path",
	)
	assert.False(
		t,
		FlavorMongoDB.IsDocumentDB(),
		"MongoDB must take the $clusterTime path",
	)
}

// TestGetClusterTimeFromSessionParsesNestedField guards the shape we expect
// from a gossiped $clusterTime document.
func TestGetClusterTimeFromSessionParsesNestedField(t *testing.T) {
	expected := bson.Timestamp{T: 1571788022, I: 2}

	raw, err := bson.Marshal(bson.D{
		{"$clusterTime", bson.D{{"clusterTime", expected}}},
	})
	require.NoError(t, err)

	ctrv, err := bson.Raw(raw).LookupErr("$clusterTime", "clusterTime")
	require.NoError(t, err)

	var actual bson.Timestamp
	require.NoError(t, ctrv.Unmarshal(&actual))
	assert.Equal(t, expected, actual)
}
