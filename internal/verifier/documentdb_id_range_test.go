package verifier

import (
	"testing"

	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestIdRangeFilterIsExact(t *testing.T) {
	oid := bsontools.ToRawValue(bson.NewObjectID())
	oid2 := bsontools.ToRawValue(bson.NewObjectID())
	intVal := bsontools.ToRawValue(int32(5))
	doubleVal := bsontools.ToRawValue(float64(9.5))
	strVal := bsontools.ToRawValue("abc")
	minKey := bsontools.ToRawValue(bson.MinKey{})
	maxKey := bsontools.ToRawValue(bson.MaxKey{})

	extremes := func(lowest, highest bson.Type) func() (option.Option[idTypeExtremes], error) {
		return func() (option.Option[idTypeExtremes], error) {
			return option.Some(idTypeExtremes{lowest: lowest, highest: highest}), nil
		}
	}

	noExtremesNeeded := func() (option.Option[idTypeExtremes], error) {
		require.FailNow(t, "should not need the collection’s _id types")
		return option.None[idTypeExtremes](), nil
	}

	emptyCollection := func() (option.Option[idTypeExtremes], error) {
		return option.None[idTypeExtremes](), nil
	}

	for _, tc := range []struct {
		label       string
		lower       bson.RawValue
		upper       bson.RawValue
		getExtremes func() (option.Option[idTypeExtremes], error)
		exact       bool
	}{
		{"both bounds ObjectID", oid, oid2, noExtremesNeeded, true},
		{"mixed numeric bounds", intVal, doubleVal, noExtremesNeeded, true},
		{"bounds in different brackets", intVal, strVal, noExtremesNeeded, false},
		{"both bounds open", minKey, maxKey, noExtremesNeeded, true},

		{"open lower, lowest _id in same bracket", minKey, oid, extremes(bson.TypeObjectID, bson.TypeObjectID), true},
		{"open lower, lowest _id in earlier bracket", minKey, oid, extremes(bson.TypeString, bson.TypeObjectID), false},

		{"open upper, highest _id in same bracket", oid, maxKey, extremes(bson.TypeObjectID, bson.TypeObjectID), true},
		{"open upper, highest _id in later bracket", oid, maxKey, extremes(bson.TypeObjectID, bson.TypeDateTime), false},

		// Only the open end matters; other brackets at the far end are
		// excluded by the bound itself.
		{"open upper, lowest _id in other bracket", oid, maxKey, extremes(bson.TypeString, bson.TypeObjectID), true},

		{"open bound, empty collection", oid, maxKey, emptyCollection, false},
		{"missing bound", bson.RawValue{}, oid, noExtremesNeeded, false},
	} {
		exact, err := idRangeFilterIsExact(tc.lower, tc.upper, tc.getExtremes)
		require.NoError(t, err, tc.label)
		assert.Equal(t, tc.exact, exact, tc.label)
	}
}

func TestIdRangeFilterIsExactPropagatesErrors(t *testing.T) {
	theErr := errors.New("server unavailable")

	_, err := idRangeFilterIsExact(
		bsontools.ToRawValue(bson.MinKey{}),
		bsontools.ToRawValue(bson.NewObjectID()),
		func() (option.Option[idTypeExtremes], error) {
			return option.None[idTypeExtremes](), theErr
		},
	)

	require.ErrorIs(t, err, theErr)
}

func TestIDRangeSelectionRequiresDocumentDBIDPartitions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		info      *util.ClusterInfo
		partition *partitions.Partition
		want      bool
	}{
		{name: "missing cluster"},
		{name: "MongoDB", info: &util.ClusterInfo{Flavor: util.FlavorMongoDB}},
		{name: "missing partition", info: &util.ClusterInfo{Flavor: util.FlavorDocumentDB}},
		{name: "natural", info: &util.ClusterInfo{Flavor: util.FlavorDocumentDB}, partition: &partitions.Partition{Natural: true}},
		{name: "capped", info: &util.ClusterInfo{Flavor: util.FlavorDocumentDB}, partition: &partitions.Partition{IsCapped: true}},
		{
			name: "bounded ObjectIDs", info: &util.ClusterInfo{Flavor: util.FlavorDocumentDB}, want: true,
			partition: &partitions.Partition{
				Key:   partitions.PartitionKey{Lower: bsontools.ToRawValue(bson.NewObjectID())},
				Upper: bsontools.ToRawValue(bson.NewObjectID()),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := NewVerifier(VerifierSettings{}, "stderr")
			got, err := v.canUseIdRangeFilter(t.Context(), nil, tc.info, tc.partition)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
