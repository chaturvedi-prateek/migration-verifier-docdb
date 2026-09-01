package uuidutil

import (
	"context"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// NamespaceAndUUID represents a collection and database name and its corresponding UUID.  It is
// used as a bridge between code which expects collection UUIDs and code which uses namespaces only.
type NamespaceAndUUID struct {
	// The collection's UUID
	UUID util.UUID

	// The collection's database name
	DBName string

	// The collection's name.
	CollName string
}

func GetCollectionNamespaceAndUUID(ctx context.Context, logger *logger.Logger, db *mongo.Database, collName string) (*NamespaceAndUUID, error) {
	binaryUUID, uuidErr := GetCollectionUUID(ctx, logger, db, collName)
	if uuidErr != nil {
		return nil, uuidErr
	}
	return &NamespaceAndUUID{
		UUID:     util.ParseBinary(binaryUUID),
		DBName:   db.Name(),
		CollName: collName,
	}, nil
}

func GetCollectionUUID(ctx context.Context, logger *logger.Logger, db *mongo.Database, collName string) (*bson.Binary, error) {
	filter := bson.D{{"name", collName}}
	opts := options.ListCollections().SetNameOnly(false)

	var collSpecs []mongo.CollectionSpecification
	err := retry.New().WithCallback(
		func(_ context.Context, ri *retry.FuncInfo) error {
			ri.Log(logger.Logger, "ListCollectionSpecifications", db.Name(), collName, "Getting collection UUID.", "")
			var driverErr error
			collSpecs, driverErr = db.ListCollectionSpecifications(ctx, filter, opts)
			return driverErr
		},
		"getting namespace %#q's specification",
		db.Name()+"."+collName,
	).Run(ctx, logger)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to list collections specification")
	}

	if len(collSpecs) != 1 {
		return nil, errors.Errorf(
			"expected exactly 1 collection matching %#q, got %d",
			db.Name()+"."+collName,
			len(collSpecs),
		)
	}

	// A nil UUID is not a programming error, so it must not panic. It happens
	// for views, which have no UUID, and for servers that do not report
	// collection UUIDs at all — DocumentDB's listCollections omits info.uuid.
	if collSpecs[0].UUID == nil {
		return nil, errors.Errorf(
			"%#q has no UUID (it may be a view, or the server may not report collection UUIDs): %v",
			db.Name()+"."+collName,
			collSpecs[0],
		)
	}

	return collSpecs[0].UUID, nil
}
