package util

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// standaloneHello mirrors the fields a real standalone mongod returns from
// `hello`, captured from a local mongod. Note the absence of both `me` and
// `$clusterTime`: a standalone resembles DocumentDB in lacking cluster-time
// gossip, which is why flavor detection requires replica-set membership
// before concluding DocumentDB.
func standaloneHello() bson.Raw {
	raw, err := bson.Marshal(bson.D{
		{"isWritablePrimary", true},
		{"maxBsonObjectSize", 16777216},
		{"localTime", bson.DateTime(0)},
		{"minWireVersion", 0},
		{"maxWireVersion", 25},
		{"readOnly", false},
		{"ok", 1},
	})
	if err != nil {
		panic(err)
	}

	return raw
}

// replicaSetHello resembles a genuine MongoDB replica set member’s response:
// it advertises membership via `me` and gossips `$clusterTime`.
func replicaSetHello() bson.Raw {
	raw, err := bson.Marshal(bson.D{
		{"topology_version", bson.D{}},
		{"hosts", bson.A{"host1:27017"}},
		{"setName", "rs0"},
		{"isWritablePrimary", true},
		{"me", "host1:27017"},
		{"$clusterTime", bson.D{
			{"clusterTime", bson.Timestamp{T: 1571788022, I: 2}},
		}},
		{"operationTime", bson.Timestamp{T: 1571788022, I: 2}},
		{"ok", 1},
	})
	if err != nil {
		panic(err)
	}

	return raw
}

// documentDBHello resembles Amazon DocumentDB: it advertises replica-set
// membership but returns no `$clusterTime`, because it does not implement
// causal consistency.
func documentDBHello() bson.Raw {
	raw, err := bson.Marshal(bson.D{
		{"hosts", bson.A{"docdb-cluster:27017"}},
		{"setName", "rs0"},
		{"isWritablePrimary", true},
		{"me", "docdb-cluster:27017"},
		{"maxBsonObjectSize", 16777216},
		{"ok", 1},
	})
	if err != nil {
		panic(err)
	}

	return raw
}

// mongosHello resembles a sharded cluster’s router, which gossips
// $clusterTime and reports `msg`.
func mongosHello() bson.Raw {
	raw, err := bson.Marshal(bson.D{
		{"isWritablePrimary", true},
		{"msg", "isdbgrid"},
		{"me", "mongos1:27017"},
		{"$clusterTime", bson.D{
			{"clusterTime", bson.Timestamp{T: 1571788022, I: 2}},
		}},
		{"ok", 1},
	})
	if err != nil {
		panic(err)
	}

	return raw
}

func TestFlavorFromHello(t *testing.T) {
	for _, curTest := range []struct {
		name     string
		hello    bson.Raw
		expected Flavor
	}{
		{
			name:     "replica set gossiping cluster time is MongoDB",
			hello:    replicaSetHello(),
			expected: FlavorMongoDB,
		},
		{
			name:     "mongos is MongoDB",
			hello:    mongosHello(),
			expected: FlavorMongoDB,
		},
		{
			name:     "replica set without cluster time is DocumentDB",
			hello:    documentDBHello(),
			expected: FlavorDocumentDB,
		},
		{
			// A standalone mongod also omits $clusterTime. Reporting it as
			// DocumentDB would wrongly disable MongoDB-only features.
			name:     "standalone mongod is MongoDB, not DocumentDB",
			hello:    standaloneHello(),
			expected: FlavorMongoDB,
		},
	} {
		t.Run(curTest.name, func(t *testing.T) {
			flavor, evidence, err := flavorFromHello(curTest.hello)

			require.NoError(t, err)
			assert.Equal(t, curTest.expected, flavor)
			assert.NotEmpty(t, evidence, "evidence should explain the verdict")
		})
	}
}
