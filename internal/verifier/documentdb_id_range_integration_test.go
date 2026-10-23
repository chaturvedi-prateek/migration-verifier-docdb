package verifier

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
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

	db := client.Database("__mv_id_range_test")

	// Cleanup runs after the test’s context is cancelled, so it needs its own.
	t.Cleanup(func() {
		cleanupCtx := context.Background()

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

	// Assume a version that uses $expr, as DocumentDB reports.
	exprClusterInfo := &util.ClusterInfo{VersionArray: []int{5, 0, 0}}

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
				actual := findIDs(t, coll, partition.GetQueryParametersForIdRange(nil))

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

	var ids []string
	for cursor.Next(t.Context()) {
		ids = append(ids, cursor.Current.Lookup("_id").String())
	}
	require.NoError(t, cursor.Err())

	return ids
}
