package uuidutil

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

// TestGetCollectionUUID_NilUUIDReturnsError ensures a collection without a
// UUID produces an error rather than taking down the process.
//
// Two real cases reach this: views, which have no UUID, and servers that do
// not report collection UUIDs at all — DocumentDB's listCollections omits
// info.uuid. Neither is a programming error, so neither may panic.
//
// A view stands in for both here, since it is the case reproducible against
// MongoDB.
func TestGetCollectionUUID_NilUUIDReturnsError(t *testing.T) {
	uri := os.Getenv("MVTEST_SRC")
	if uri == "" {
		t.Skip("TestGetCollectionUUID_NilUUIDReturnsError requires `MVTEST_SRC` in environment.")
	}

	ctx := t.Context()

	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	require.NoError(t, err)

	db := client.Database("__mv_uuid_test")

	t.Cleanup(func() {
		cleanupCtx := context.Background()

		assert.NoError(t, db.Drop(cleanupCtx))
		assert.NoError(t, client.Disconnect(cleanupCtx))
	})

	require.NoError(t, db.Drop(ctx))

	_, err = db.Collection("real").InsertOne(ctx, bson.D{{"_id", 1}})
	require.NoError(t, err)

	log := logger.NewDefaultLogger()

	// Whether a plain collection has a UUID is a server-behavior difference,
	// not a test parameter: MongoDB reports one, DocumentDB does not. Branch on
	// what this server actually did so the test is meaningful against both.
	uuid, err := GetCollectionUUID(ctx, log, db, "real")

	if err != nil {
		t.Run("a server without collection UUIDs errors rather than exiting", func(t *testing.T) {
			assert.Nil(t, uuid)
			assert.ErrorContains(t, err, "UUID")
			t.Logf("server reports no collection UUIDs: %v", err)
		})
	} else {
		require.NotNil(t, uuid, "a successful lookup must return a UUID")

		// Views have no UUID, which is the other route to a nil UUID. Only
		// MongoDB supports them; DocumentDB rejects the create.
		t.Run("a view yields an error rather than panicking", func(t *testing.T) {
			if err := db.CreateView(ctx, "aview", "real", mongo.Pipeline{}); err != nil {
				t.Skipf("server does not support views: %v", err)
			}

			assert.NotPanics(t, func() {
				uuid, err := GetCollectionUUID(ctx, log, db, "aview")

				require.Error(t, err, "a view has no UUID")
				assert.Nil(t, uuid)
				assert.ErrorContains(t, err, "UUID")
			})
		})
	}

	t.Run("a missing collection yields an error", func(t *testing.T) {
		_, err := GetCollectionUUID(ctx, log, db, "nonexistent")

		require.Error(t, err)
		assert.ErrorContains(t, err, "nonexistent")
	})
}
