package util

import (
	"context"
	"os"
	"testing"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// TestFlavorAgainstLiveServer exercises flavor detection and the flavor's
// timestamp source against whatever server MVTEST_SRC points at.
//
// It asserts the two must agree rather than hard-coding an expected flavor, so
// it is meaningful against both MongoDB and DocumentDB. Set
// MVTEST_EXPECT_FLAVOR to assert a specific one.
func TestFlavorAgainstLiveServer(t *testing.T) {
	uri := os.Getenv("MVTEST_SRC")
	if uri == "" {
		t.Skip("TestFlavorAgainstLiveServer requires `MVTEST_SRC` in environment.")
	}

	ctx := t.Context()

	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	require.NoError(t, err)

	t.Cleanup(func() {
		assert.NoError(t, client.Disconnect(context.Background()))
	})

	flavor, evidence, err := DetectFlavor(ctx, client)
	require.NoError(t, err)

	t.Logf("detected flavor %q because: %s", flavor, evidence)

	assert.Contains(t, Flavors, flavor, "detection must yield a known flavor")

	if expected := os.Getenv("MVTEST_EXPECT_FLAVOR"); expected != "" {
		assert.Equal(
			t,
			Flavor(expected),
			flavor,
			"detected flavor should match MVTEST_EXPECT_FLAVOR",
		)
	}

	info, err := GetClusterInfo(ctx, logger.NewDefaultLogger(), client)
	require.NoError(t, err)

	t.Logf("cluster info: version=%v topology=%s flavor=%s",
		info.VersionArray, info.Topology, info.Flavor)

	assert.Equal(t, flavor, info.Flavor, "GetClusterInfo must carry the detected flavor")

	t.Run("the flavor's timestamp source works", func(t *testing.T) {
		// A standalone mongod gossips no $clusterTime, so the MongoDB path
		// cannot produce a timestamp there. That is pre-existing behavior and
		// not a supported source configuration anyway: change streams require
		// a replica set.
		if info.Topology == TopologyStandalone {
			t.Skip("standalone servers gossip no cluster time")
		}

		sess, err := client.StartSession()
		require.NoError(t, err)
		defer sess.EndSession(ctx)

		sctx := mongo.NewSessionContext(ctx, sess)

		// Any command populates the session's timestamps. This must be a real
		// command, not just a connection, or the session has seen nothing yet.
		require.NoError(
			t,
			client.Database("admin").RunCommand(sctx, bson.D{{"ping", 1}}).Err(),
		)

		ts, err := GetSessionTimestamp(sess, flavor)
		require.NoError(
			t,
			err,
			"a %s session must yield a timestamp; DocumentDB reads operationTime, MongoDB reads $clusterTime",
			flavor,
		)

		assert.NotZero(t, ts.T, "timestamp should be a real clock value")
		t.Logf("session timestamp for %s: %+v", flavor, ts)
	})

	t.Run("an explicit flavor overrides detection", func(t *testing.T) {
		// Force the opposite of whatever was detected and confirm it sticks.
		forced := FlavorDocumentDB
		if flavor == FlavorDocumentDB {
			forced = FlavorMongoDB
		}

		info, err := GetClusterInfoWithFlavor(
			ctx,
			logger.NewDefaultLogger(),
			client,
			forced,
		)
		require.NoError(t, err)

		assert.Equal(t, forced, info.Flavor, "--srcFlavor must win over detection")
	})
}
