package verifier

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

// TestIdRangeFilterMatchesExprFilter checks, against a live server, that
// whenever idRangeFilterIsExact approves plain _id range predicates for a
// partition, they return exactly the documents that the $expr predicates do.
func TestIdRangeFilterMatchesExprFilter(t *testing.T) {
	uri := os.Getenv("MVTEST_SRC")
	if uri == "" {
		t.Skip("TestIdRangeFilterMatchesExprFilter requires `MVTEST_SRC` in environment.")
	}

	ctx := t.Context()

	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	require.NoError(t, err)

	db := client.Database("__mv_id_range_test_" + bson.NewObjectID().Hex())

	// Cleanup runs after the test’s context is cancelled, so it needs its own.
	t.Cleanup(func() {
		cleanupCtx := context.WithoutCancel(t.Context())

		assert.NoError(t, db.Drop(cleanupCtx))
		assert.NoError(t, client.Disconnect(cleanupCtx))
	})

	require.NoError(t, db.Drop(ctx))

	oids := lo.Times(5, func(_ int) bson.ObjectID { return bson.NewObjectID() })
	baseTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	var ids []any
	for i := range 5 {
		ids = append(
			ids,
			int32(i),
			float64(i)+0.5,
			string(rune('a'+i)),
			oids[i],
			bson.NewDateTimeFromTime(baseTime.Add(time.Duration(i)*time.Hour)),
		)
	}

	uniformColl := db.Collection("uniform")
	mixedColl := db.Collection("mixed")

	_, err = uniformColl.InsertMany(ctx, lo.Map(oids, func(oid bson.ObjectID, _ int) any {
		return bson.D{{"_id", oid}}
	}))
	require.NoError(t, err)

	_, err = mixedColl.InsertMany(ctx, lo.Map(ids, func(id any, _ int) any {
		return bson.D{{"_id", id}}
	}))
	require.NoError(t, err)

	minKey := bsontools.ToRawValue(bson.MinKey{})
	maxKey := bsontools.ToRawValue(bson.MaxKey{})

	bounds := []bson.RawValue{minKey, maxKey}
	for _, id := range ids {
		bounds = append(bounds, lo.Must(mbson.ConvertToRawValue(id)))
	}

	// Use the production DocumentDB query-selection path against the test
	// server, and compare it with the non-type-bracketed reference filter.
	exprClusterInfo := &util.ClusterInfo{VersionArray: []int{5, 0, 0}}
	docDBClusterInfo := &util.ClusterInfo{VersionArray: []int{5, 0, 0}, Flavor: util.FlavorDocumentDB}
	v := NewVerifier(VerifierSettings{}, "stderr")

	for _, coll := range []*mongo.Collection{uniformColl, mixedColl} {
		getExtremes := func() (option.Option[idTypeExtremes], error) {
			lowest, err := findExtremeIDType(ctx, coll, 1)
			if err != nil {
				return option.None[idTypeExtremes](), err
			}

			highest, err := findExtremeIDType(ctx, coll, -1)
			if err != nil {
				return option.None[idTypeExtremes](), err
			}

			return option.Some(idTypeExtremes{
				lowest:  lowest.MustGet(),
				highest: highest.MustGet(),
			}), nil
		}

		approved := 0

		for _, lower := range bounds {
			for _, upper := range bounds {
				if lower.Type == bson.TypeMaxKey || upper.Type == bson.TypeMinKey {
					continue
				}

				exact, err := idRangeFilterIsExact(lower, upper, getExtremes)
				require.NoError(t, err)

				if !exact {
					continue
				}

				approved++

				partition := &partitions.Partition{
					Key:   partitions.PartitionKey{Lower: lower},
					Ns:    &partitions.Namespace{DB: db.Name(), Coll: coll.Name()},
					Upper: upper,
				}

				exprParams, err := partition.GetQueryParameters(exprClusterInfo, nil)
				require.NoError(t, err)

				expected := findIDs(t, coll, exprParams)
				actual := findIDsWithVerifier(t, v, coll, docDBClusterInfo, partition)

				assert.Equal(
					t,
					expected,
					actual,
					"%s: range [%s, %s]",
					coll.Name(),
					lower,
					upper,
				)
			}
		}

		assert.Positive(t, approved, "%s: some ranges should use plain predicates", coll.Name())
	}
}

func findIDs(
	t *testing.T,
	coll *mongo.Collection,
	params partitions.PartitionQueryParameters,
) []string {
	cmd := append(bson.D{{"find", coll.Name()}}, params.ToFindOptions()...)

	cursor, err := coll.Database().RunCommandCursor(t.Context(), cmd)
	require.NoError(t, err)
	defer cursor.Close(t.Context())

	var ids []string
	for cursor.Next(t.Context()) {
		ids = append(ids, cursor.Current.Lookup("_id").String())
	}
	require.NoError(t, cursor.Err())

	return ids
}

func findIDsWithVerifier(
	t *testing.T,
	v *Verifier,
	coll *mongo.Collection,
	info *util.ClusterInfo,
	partition *partitions.Partition,
) []string {
	sess, err := coll.Database().Client().StartSession(options.Session().SetCausalConsistency(false))
	require.NoError(t, err)
	defer sess.EndSession(t.Context())
	task := &tasks.Task{
		PrimaryKey:  bson.NewObjectID(),
		QueryFilter: tasks.QueryFilter{Namespace: FullName(coll), Partition: partition},
	}
	cursor, err := v.getDocumentsCursor(
		mongo.NewSessionContext(t.Context(), sess), coll, info, bson.Timestamp{T: 1, I: 1}, task,
		option.Some(readpref.Primary()),
	)
	require.NoError(t, err)
	defer cursor.Close(t.Context())
	var ids []string
	for cursor.Next(t.Context()) {
		ids = append(ids, cursor.Current.Lookup("_id").String())
	}
	require.NoError(t, cursor.Err())
	return ids
}

func TestDocumentDBQuerySelectionUsesCurrentCollectionTypes(t *testing.T) {
	uri := os.Getenv("MVTEST_SRC")
	if uri == "" {
		t.Skip("requires MVTEST_SRC")
	}
	var mu sync.Mutex
	var lastFilter bson.Raw
	monitor := &event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
		if e.CommandName == "find" && e.Command.Lookup("comment").Type != 0 {
			mu.Lock()
			defer mu.Unlock()
			lastFilter = append(bson.Raw(nil), e.Command.Lookup("filter").Document()...)
		}
	}}
	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetMonitor(monitor))
	require.NoError(t, err)
	db := client.Database("__mv_id_range_selection_" + bson.NewObjectID().Hex())
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		assert.NoError(t, db.Drop(ctx))
		assert.NoError(t, client.Disconnect(ctx))
	})
	coll := db.Collection("docs")
	low := bson.NewObjectID()
	high := bson.NewObjectID()
	_, err = coll.InsertMany(t.Context(), []any{bson.D{{"_id", low}}, bson.D{{"_id", high}}})
	require.NoError(t, err)
	v := NewVerifier(VerifierSettings{}, "stderr")
	info := &util.ClusterInfo{Flavor: util.FlavorDocumentDB, VersionArray: []int{5, 0, 0}}
	p := &partitions.Partition{
		Key:   partitions.PartitionKey{Lower: bsontools.ToRawValue(bson.MinKey{})},
		Upper: bsontools.ToRawValue(high),
		Ns:    &partitions.Namespace{DB: db.Name(), Coll: coll.Name()},
	}
	readFilter := func() string {
		mu.Lock()
		defer mu.Unlock()
		return lastFilter.String()
	}
	require.Len(t, findIDsWithVerifier(t, v, coll, info, p), 2)
	require.NotContains(t, readFilter(), "$expr")

	_, err = coll.InsertOne(t.Context(), bson.D{{"_id", "earlier-type"}})
	require.NoError(t, err)
	require.Len(t, findIDsWithVerifier(t, v, coll, info, p), 3)
	require.Contains(t, readFilter(), "$expr", "an open range must cover the newly present type")

	// A different source collection with the same namespace must also be
	// evaluated from its current contents, rather than prior cached extrema.
	require.NoError(t, coll.Drop(t.Context()))
	_, err = coll.InsertMany(t.Context(), []any{bson.D{{"_id", low}}, bson.D{{"_id", high}}})
	require.NoError(t, err)
	require.Len(t, findIDsWithVerifier(t, v, coll, info, p), 2)
	require.NotContains(t, readFilter(), "$expr")

	info.Flavor = util.FlavorMongoDB
	require.Len(t, findIDsWithVerifier(t, v, coll, info, p), 2)
	require.Contains(t, readFilter(), "$expr", "MongoDB retains its original range filter")
}
