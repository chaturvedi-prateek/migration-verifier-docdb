package verifier

import (
	"context"
	"math"
	"slices"

	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// createPartitionTasksWithIndexWalk partitions a collection by walking its
// _id index, which is the only partitioning strategy available against
// DocumentDB: $sampleRate, $bucketAuto, and $natural are all unsupported
// there, and $sample’s cost characteristics are undocumented.
//
// For each boundary we ask the server for the _id that sits
// docsPerPartition documents past the previous boundary:
//
//	find({_id: {$gt: prev}}).sort({_id: 1}).skip(n-1).limit(1).project({_id: 1})
//
// Hinting and projecting _id makes this a covered index scan, so the server
// walks index entries without fetching documents, and each call transfers a
// single _id. Across the whole collection the server therefore makes one pass
// over the _id index. That is more work than sampling, but it is predictable,
// needs no server features beyond an indexed range scan, and is small next to
// the full document read that verification itself performs.
func (verifier *Verifier) createPartitionTasksWithIndexWalk(
	ctx context.Context,
	task *tasks.Task,
	shardFields []string,
) (int, error) {
	srcNs := FullName(verifier.srcClientCollection(task))

	var partitionsCount int

	err := retry.New().WithCallback(
		func(ctx context.Context, fi *retry.FuncInfo) error {
			var err error

			partitionsCount, err = verifier.createPartitionTasksWithIndexWalkRetryable(
				ctx,
				fi,
				task,
				shardFields,
			)

			return err
		},
		"partitioning %#q by walking its %#q index",
		srcNs,
		"_id",
	).Run(ctx, verifier.logger)

	return partitionsCount, err
}

func (verifier *Verifier) createPartitionTasksWithIndexWalkRetryable(
	ctx context.Context,
	fi *retry.FuncInfo,
	task *tasks.Task,
	shardFields []string,
) (int, error) {
	srcColl := verifier.srcClientCollection(task)
	srcNs := FullName(srcColl)
	dstNs := FullName(verifier.dstClientCollection(task))

	collBytes, docsCount, isCapped, err := partitions.GetSizeAndDocumentCount(
		ctx,
		verifier.logger,
		srcColl,
	)
	if err != nil {
		return 0, errors.Wrapf(err, "getting %#q’s size", srcNs)
	}

	partitionsCount := 0

	createAndInsertPartition := func(lowerBound, upperBound bson.RawValue) error {
		partition := partitions.Partition{
			Key: partitions.PartitionKey{
				Lower: lowerBound,
			},
			Ns: &partitions.Namespace{
				DB:   srcColl.Database().Name(),
				Coll: srcColl.Name(),
			},
			Upper: upperBound,

			// $natural ordering, which the comparison path uses for capped
			// collections, doesn’t exist on DocumentDB. Comparing a capped
			// collection in _id order is still correct for verification —
			// natural order only matters when reproducing insertion order —
			// so we deliberately don’t mark these as capped.
			IsCapped: false,
		}

		_, err := verifier.InsertPartitionVerificationTask(
			ctx,
			&partition,
			shardFields,
			dstNs,
		)
		if err != nil {
			return errors.Wrapf(
				err,
				"inserting partition task for namespace %#q",
				srcNs,
			)
		}

		partitionsCount++

		fi.NoteSuccess("inserted partition #%d", partitionsCount)

		return nil
	}

	if isCapped {
		verifier.logger.Warn().
			Str("namespace", srcNs).
			Msg("Comparing a capped collection in _id order because the source lacks $natural ordering.")
	}

	docsPerPartition := docsPerPartitionFor(
		collBytes,
		docsCount,
		verifier.partitionSizeInBytes,
	)

	lowerBound := bsontools.ToRawValue(bson.MinKey{})

	lowerBoundOpt, err := verifier.findLatestPartitionUpperBound(ctx, srcNs)
	if err != nil {
		return 0, err
	}

	if resumeFrom, has := lowerBoundOpt.Get(); has {
		verifier.logger.Info().
			Any("resumeFrom", resumeFrom).
			Str("namespace", srcNs).
			Msg("Resuming partitioning from last-created partition’s upper bound.")

		lowerBound = resumeFrom
	}

	verifier.logger.Debug().
		Str("namespace", srcNs).
		Int64("docsPerPartition", docsPerPartition).
		Int64("documentsCount", int64(docsCount)).
		Int64("collectionBytes", int64(collBytes)).
		Msg("Walking _id index to find partition boundaries.")

	for {
		upperBoundOpt, err := findNextIDBoundary(
			ctx,
			srcColl,
			lowerBound,
			docsPerPartition,
		)
		if err != nil {
			return 0, errors.Wrapf(err, "finding %#q’s next partition boundary", srcNs)
		}

		upperBound, has := upperBoundOpt.Get()
		if !has {
			// Fewer than docsPerPartition documents remain, so the final
			// partition below covers the rest.
			break
		}

		if err := createAndInsertPartition(lowerBound, upperBound); err != nil {
			return 0, err
		}

		lowerBound = upperBound
	}

	// The final partition is unbounded above. It also catches any _ids whose
	// BSON type sorts after the walked type bracket: the walk’s range query is
	// type-bracketed, but a partition’s own filter is not (see FilterIdBounds),
	// so no document can escape verification even with mixed-type _ids.
	if err := createAndInsertPartition(
		lowerBound,
		bsontools.ToRawValue(bson.MaxKey{}),
	); err != nil {
		return 0, err
	}

	return partitionsCount, nil
}

// docsPerPartitionFor returns how many documents each partition should span in
// order to approximate the configured partition size in bytes. It returns at
// least 1.
func docsPerPartitionFor(
	collBytes types.ByteCount,
	docsCount types.DocumentCount,
	partitionSizeInBytes types.ByteCount,
) int64 {
	if docsCount <= 0 || collBytes <= 0 {
		return math.MaxInt64
	}

	idealNumPartitions := util.DivideToF64(collBytes, partitionSizeInBytes)

	if idealNumPartitions <= 1 {
		return math.MaxInt64
	}

	docsPerPartition := int64(math.Ceil(util.DivideToF64(docsCount, idealNumPartitions)))

	return max(docsPerPartition, 1)
}

// findNextIDBoundary returns the _id of the document that sits
// docsPerPartition documents after `after` in _id order, or None if fewer than
// that many documents remain.
//
// The hint and _id-only projection make this a covered index scan: the server
// walks index entries and never fetches a document, and only one _id crosses
// the wire per call.
func findNextIDBoundary(
	ctx context.Context,
	coll *mongo.Collection,
	after bson.RawValue,
	docsPerPartition int64,
) (option.Option[bson.RawValue], error) {
	if docsPerPartition == math.MaxInt64 {
		// The whole collection fits in one partition, so there is no boundary
		// to look for. Skipping the query avoids an overflowing skip value.
		return option.None[bson.RawValue](), nil
	}

	filter := bson.D{}
	if after.Type != bson.TypeMinKey {
		filter = bson.D{{"_id", bson.D{{"$gt", after}}}}
	}

	res := coll.FindOne(
		ctx,
		filter,
		options.FindOne().
			SetSort(bson.D{{"_id", 1}}).
			SetProjection(bson.D{{"_id", 1}}).
			SetHint(bson.D{{"_id", 1}}).
			SetSkip(docsPerPartition-1),
	)

	raw, err := res.Raw()
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return option.None[bson.RawValue](), nil
		}

		return option.None[bson.RawValue](), errors.Wrapf(
			err,
			"failed to read %#q past %v",
			"_id",
			after,
		)
	}

	found, err := raw.LookupErr("_id")
	if err != nil {
		return option.None[bson.RawValue](), errors.Wrapf(
			err,
			"failed to extract %#q from partition boundary (%v)",
			"_id",
			raw,
		)
	}

	// Copy the value out of the response buffer so it stays valid once the
	// buffer is reused.
	return option.Some(bson.RawValue{
		Type:  found.Type,
		Value: slices.Clone(found.Value),
	}), nil
}
