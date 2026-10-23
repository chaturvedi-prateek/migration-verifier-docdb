package verifier

import (
	"testing"

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
