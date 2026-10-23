package verifier

import (
	"context"

	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// idTypeExtremes records the BSON types of a collection’s lowest and highest
// _id values.
type idTypeExtremes struct {
	lowest  bson.Type
	highest bson.Type
}

// canUseIdRangeFilter reports whether the verifier should fetch the given
// partition’s documents with plain _id range predicates rather than the $expr
// predicates from partitions.FilterIdBounds.
//
// Some DocumentDB query plans do not efficiently bound $expr index scans.
// Plain range predicates expose the bounds directly to the query planner;
// their performance must still be verified on the deployed engine version.
//
// Plain range predicates are type-bracketed, though, so this returns true only
// when they match exactly the same documents as the $expr predicates.
func (verifier *Verifier) canUseIdRangeFilter(
	ctx context.Context,
	coll *mongo.Collection,
	clusterInfo *util.ClusterInfo,
	partition *partitions.Partition,
) (bool, error) {
	if clusterInfo == nil || !clusterInfo.Flavor.IsDocumentDB() {
		return false, nil
	}

	if partition == nil || partition.IsCapped || partition.Natural {
		return false, nil
	}

	return idRangeFilterIsExact(
		partition.Key.Lower,
		partition.Upper,
		func() (option.Option[idTypeExtremes], error) {
			return verifier.getIDTypeExtremes(ctx, coll)
		},
	)
}

// idRangeFilterIsExact reports whether partitions.RangeIdPredicates for the
// given inclusive bounds match exactly the same documents as
// partitions.FilterIdBounds.
//
// The BSON sort order groups each comparison bracket contiguously. So when
// both bounds share a bracket, every _id between them shares it too, and type
// bracketing excludes nothing. When one bound is open (MinKey or MaxKey), the
// collection’s lowest (or highest) _id must also share the other bound’s
// bracket; getExtremes supplies that and is called only in this case.
//
// Documents inserted after getExtremes runs don’t affect correctness: the
// change reader observes their insertion, so they are rechecked by _id.
func idRangeFilterIsExact(
	lower, upper bson.RawValue,
	getExtremes func() (option.Option[idTypeExtremes], error),
) (bool, error) {
	// A partition always has both bounds. If either is missing, be safe.
	if lower.Type == 0 || upper.Type == 0 {
		return false, nil
	}

	lowerOpen := lower.Type == bson.TypeMinKey
	upperOpen := upper.Type == bson.TypeMaxKey

	switch {
	case lowerOpen && upperOpen:
		// With no _id predicate at all, both filters match every document.
		return true, nil
	case !lowerOpen && !upperOpen:
		return partitions.SameSortBracket(lower.Type, upper.Type), nil
	}

	extremesOpt, err := getExtremes()
	if err != nil {
		return false, err
	}

	extremes, has := extremesOpt.Get()
	if !has {
		return false, nil
	}

	if lowerOpen {
		return partitions.SameSortBracket(extremes.lowest, upper.Type), nil
	}

	return partitions.SameSortBracket(extremes.highest, lower.Type), nil
}

// getIDTypeExtremes returns the types of the collection’s lowest and highest
// _id values, or None if the collection is empty. Read them afresh for each
// open-ended partition so a new source or verification never reuses stale
// type information. Bounded partitions do not need these lookups.
func (verifier *Verifier) getIDTypeExtremes(
	ctx context.Context,
	coll *mongo.Collection,
) (option.Option[idTypeExtremes], error) {
	ns := FullName(coll)

	lowestOpt, err := findExtremeIDType(ctx, coll, 1)
	if err != nil {
		return option.None[idTypeExtremes](), errors.Wrapf(err, "finding %#q’s lowest _id", ns)
	}

	highestOpt, err := findExtremeIDType(ctx, coll, -1)
	if err != nil {
		return option.None[idTypeExtremes](), errors.Wrapf(err, "finding %#q’s highest _id", ns)
	}

	lowest, hasLowest := lowestOpt.Get()
	highest, hasHighest := highestOpt.Get()

	if !hasLowest || !hasHighest {
		return option.None[idTypeExtremes](), nil
	}

	extremes := idTypeExtremes{lowest: lowest, highest: highest}
	verifier.logger.Debug().
		Str("namespace", ns).
		Stringer("lowestIDType", lowest).
		Stringer("highestIDType", highest).
		Msg("Found the source collection’s _id type range.")

	return option.Some(extremes), nil
}

// findExtremeIDType returns the BSON type of the collection’s first _id in
// the given sort direction (1 or -1), or None if the collection is empty.
func findExtremeIDType(
	ctx context.Context,
	coll *mongo.Collection,
	direction int,
) (option.Option[bson.Type], error) {
	raw, err := coll.FindOne(
		ctx,
		bson.D{},
		options.FindOne().
			SetSort(bson.D{{"_id", direction}}).
			SetProjection(bson.D{{"_id", 1}}).
			SetHint(bson.D{{"_id", 1}}),
	).Raw()
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return option.None[bson.Type](), nil
		}

		return option.None[bson.Type](), err
	}

	id, err := raw.LookupErr("_id")
	if err != nil {
		return option.None[bson.Type](), errors.Wrapf(err, "reading %#q from %v", "_id", raw)
	}

	return option.Some(id.Type), nil
}
