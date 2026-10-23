package partitions

import (
	"testing"

	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestSameSortBracket(t *testing.T) {
	for _, tc := range []struct {
		a, b bson.Type
		same bool
	}{
		{bson.TypeObjectID, bson.TypeObjectID, true},
		{bson.TypeInt32, bson.TypeDouble, true},
		{bson.TypeInt64, bson.TypeDecimal128, true},
		{bson.TypeString, bson.TypeSymbol, true},
		{bson.TypeInt32, bson.TypeString, false},
		{bson.TypeObjectID, bson.TypeString, false},
		{bson.TypeDateTime, bson.TypeTimestamp, false},
		{bson.TypeNull, bson.TypeObjectID, false},
	} {
		assert.Equal(
			t,
			tc.same,
			SameSortBracket(tc.a, tc.b),
			"%s vs. %s",
			tc.a,
			tc.b,
		)
	}
}

func TestRangeIdPredicates(t *testing.T) {
	lower := bsontools.ToRawValue(int32(10))
	upper := bsontools.ToRawValue(int32(20))
	minKey := bsontools.ToRawValue(bson.MinKey{})
	maxKey := bsontools.ToRawValue(bson.MaxKey{})

	assert.Equal(
		t,
		[]bson.D{
			{{"_id", bson.D{{"$gte", lower}}}},
			{{"_id", bson.D{{"$lte", upper}}}},
		},
		RangeIdPredicates(lower, upper),
		"both bounds",
	)

	assert.Equal(
		t,
		[]bson.D{{{"_id", bson.D{{"$lte", upper}}}}},
		RangeIdPredicates(minKey, upper),
		"open lower bound",
	)

	assert.Equal(
		t,
		[]bson.D{{{"_id", bson.D{{"$gte", lower}}}}},
		RangeIdPredicates(lower, maxKey),
		"open upper bound",
	)

	assert.Empty(t, RangeIdPredicates(minKey, maxKey), "both bounds open")
}

func TestGetQueryParametersForIdRangeUnsafe(t *testing.T) {
	lower := bsontools.ToRawValue(bson.NewObjectID())
	upper := bsontools.ToRawValue(bson.NewObjectID())
	extraFilter := bson.D{{"x", 1}}

	partition := &Partition{
		Key:   PartitionKey{Lower: lower},
		Ns:    &Namespace{DB: "db", Coll: "coll"},
		Upper: upper,
	}

	findOptions := partition.GetQueryParametersForIdRangeUnsafe(bson.A{extraFilter}).ToFindOptions()

	require.Equal(
		t,
		bson.D{
			{"filter", bson.D{{"$and", bson.A{
				extraFilter,
				bson.D{{"_id", bson.D{{"$gte", lower}}}},
				bson.D{{"_id", bson.D{{"$lte", upper}}}},
			}}}},
			{"hint", bson.D{{"_id", 1}}},
			{"sort", bson.D{{"_id", 1}}},
		},
		findOptions,
	)

	partition.IsCapped = true
	findOptions = partition.GetQueryParametersForIdRangeUnsafe(nil).ToFindOptions()

	require.Equal(
		t,
		bson.D{{"sort", bson.D{{"$natural", 1}}}},
		findOptions,
		"capped collections should use natural order with no _id bounds",
	)
}
