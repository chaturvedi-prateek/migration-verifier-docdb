package verifier

import (
	"context"
	"os"
	"testing"

	"github.com/10gen/migration-verifier/mslices"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// TestFindNextIDBoundary exercises the _id-index walk against a live server.
//
// The walk deliberately uses only an indexed range scan with skip/limit, so it
// behaves identically on MongoDB and DocumentDB. That lets us validate the
// algorithm on MongoDB even though it exists for DocumentDB’s sake.
func TestFindNextIDBoundary(t *testing.T) {
	uri := os.Getenv("MVTEST_SRC")
	if uri == "" {
		t.Skip("TestFindNextIDBoundary requires `MVTEST_SRC` in environment.")
	}

	ctx := t.Context()

	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	require.NoError(t, err)

	db := client.Database("__mv_partition_walk_test")

	// Cleanup runs after the test’s context is cancelled, so it needs its own.
	t.Cleanup(func() {
		cleanupCtx := context.Background()

		assert.NoError(t, db.Drop(cleanupCtx))
		assert.NoError(t, client.Disconnect(cleanupCtx))
	})

	require.NoError(t, db.Drop(ctx))

	const docCount = 1000

	coll := db.Collection("docs")

	docs := make([]any, 0, docCount)
	for i := range docCount {
		docs = append(docs, bson.D{{"_id", i}, {"payload", "x"}})
	}

	_, err = coll.InsertMany(ctx, docs)
	require.NoError(t, err)

	minKey := bson.RawValue{Type: bson.TypeMinKey, Value: mslices.Of[byte]()}

	t.Run("walks to evenly spaced boundaries", func(t *testing.T) {
		const docsPerPartition = 100

		var boundaries []int32

		cur := minKey
		for {
			nextOpt, err := findNextIDBoundary(ctx, coll, cur, docsPerPartition)
			require.NoError(t, err)

			next, has := nextOpt.Get()
			if !has {
				break
			}

			var id int32
			require.NoError(t, next.Unmarshal(&id))
			boundaries = append(boundaries, id)

			cur = next
		}

		// Starting from MinKey the 100th document is _id 99, then each
		// subsequent hop advances by exactly docsPerPartition.
		assert.Equal(
			t,
			[]int32{99, 199, 299, 399, 499, 599, 699, 799, 899, 999},
			boundaries,
			"boundaries should be evenly spaced by document count",
		)
	})

	t.Run("reports no boundary when the remainder is too small", func(t *testing.T) {
		// Only 1000 documents exist, so there is no 5000th document.
		nextOpt, err := findNextIDBoundary(ctx, coll, minKey, 5000)
		require.NoError(t, err)

		_, has := nextOpt.Get()
		assert.False(t, has, "a too-large skip should yield no boundary")
	})

	t.Run("an empty collection yields no boundary", func(t *testing.T) {
		empty := db.Collection("empty")
		require.NoError(t, empty.Drop(ctx))

		nextOpt, err := findNextIDBoundary(ctx, empty, minKey, 10)
		require.NoError(t, err)

		_, has := nextOpt.Get()
		assert.False(t, has)
	})

	t.Run("partitions cover every document exactly once at the boundaries", func(t *testing.T) {
		const docsPerPartition = 250

		// Reproduce the partitioning loop and confirm the resulting ranges
		// cover the whole collection. Bounds are inclusive at both ends, so
		// adjacent partitions share their boundary document; that overlap is
		// intentional, since re-comparing a document is harmless but missing
		// one is not.
		type rng struct{ lo, hi bson.RawValue }

		var ranges []rng

		cur := minKey
		for {
			nextOpt, err := findNextIDBoundary(ctx, coll, cur, docsPerPartition)
			require.NoError(t, err)

			next, has := nextOpt.Get()
			if !has {
				break
			}

			ranges = append(ranges, rng{cur, next})
			cur = next
		}

		maxKey := bson.RawValue{Type: bson.TypeMaxKey, Value: mslices.Of[byte]()}
		ranges = append(ranges, rng{cur, maxKey})

		seen := map[int32]bool{}

		for _, r := range ranges {
			filter := bson.D{}

			and := bson.A{}
			if r.lo.Type != bson.TypeMinKey {
				and = append(and, bson.D{{"_id", bson.D{{"$gte", r.lo}}}})
			}
			if r.hi.Type != bson.TypeMaxKey {
				and = append(and, bson.D{{"_id", bson.D{{"$lte", r.hi}}}})
			}
			if len(and) > 0 {
				filter = bson.D{{"$and", and}}
			}

			cursor, err := coll.Find(ctx, filter)
			require.NoError(t, err)

			var found []struct {
				ID int32 `bson:"_id"`
			}
			require.NoError(t, cursor.All(ctx, &found))

			for _, doc := range found {
				seen[doc.ID] = true
			}
		}

		assert.Len(t, seen, docCount, "every document must fall in some partition")
	})
}
