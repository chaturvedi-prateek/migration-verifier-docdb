package util

import (
	"cmp"
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/mmongo"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

type ClusterTopology string

// Flavor identifies which server implementation a connection speaks to.
// Amazon DocumentDB emulates MongoDB’s wire protocol and reports a MongoDB
// version number, but lacks many MongoDB features. Version checks alone
// therefore can’t gate capabilities; callers must consult the flavor too.
type Flavor string

const (
	FlavorMongoDB    Flavor = "mongodb"
	FlavorDocumentDB Flavor = "documentdb"
)

// Flavors are the values the flavor CLI flag accepts, plus FlavorAuto.
var Flavors = mslices.Of(FlavorMongoDB, FlavorDocumentDB)

// FlavorAuto tells GetClusterInfoWithFlavor to detect the flavor rather than
// forcing one.
const FlavorAuto Flavor = "auto"

// IsDocumentDB reports whether the cluster is Amazon DocumentDB.
func (f Flavor) IsDocumentDB() bool {
	return f == FlavorDocumentDB
}

type ClusterInfo struct {
	VersionArray []int
	Topology     ClusterTopology
	Flavor       Flavor
}

// ClusterHasBSONSize indicates whether a cluster with the given
// major & minor version numbers supports the $bsonSize aggregation operator.
func ClusterHasBSONSize(va [2]int) bool {
	major := va[0]

	if major == 4 {
		return va[1] >= 4
	}

	return major > 4
}

func ClusterHasCurrentOpIdleCursors(va [2]int) bool {
	major := va[0]

	if major == 4 {
		return va[1] >= 2
	}

	return major > 4
}

var ClusterHasChangeStreamStartAfter = ClusterHasCurrentOpIdleCursors

const (
	TopologySharded    ClusterTopology = "sharded"
	TopologyReplset    ClusterTopology = "replset"
	TopologyStandalone ClusterTopology = "standalone"
)

func CmpMinorVersions(a, b [2]int) int {
	return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1]))
}

func GetClusterInfo(ctx context.Context, logger *logger.Logger, client *mongo.Client) (ClusterInfo, error) {
	return GetClusterInfoWithFlavor(ctx, logger, client, FlavorAuto)
}

// GetClusterInfoWithFlavor is GetClusterInfo but lets the caller force a
// server flavor rather than detecting it. Pass FlavorAuto to detect.
func GetClusterInfoWithFlavor(
	ctx context.Context,
	logger *logger.Logger,
	client *mongo.Client,
	flavorOverride Flavor,
) (ClusterInfo, error) {
	va, err := mmongo.GetVersionArray(ctx, client)
	if err != nil {
		return ClusterInfo{}, errors.Wrap(err, "failed to fetch version array")
	}

	topology, err := getTopology(ctx, client)
	if err != nil {
		return ClusterInfo{}, errors.Wrapf(err, "failed to learn topology")
	}

	flavor := flavorOverride

	if flavor == FlavorAuto {
		var evidence string

		flavor, evidence, err = DetectFlavor(ctx, client)
		if err != nil {
			return ClusterInfo{}, errors.Wrap(err, "failed to detect server flavor")
		}

		logger.Debug().
			Str("flavor", string(flavor)).
			Str("evidence", evidence).
			Msg("Detected server flavor.")
	} else {
		logger.Debug().
			Str("flavor", string(flavor)).
			Msg("Server flavor was set explicitly; skipping detection.")
	}

	return ClusterInfo{
		VersionArray: va[:],
		Topology:     topology,
		Flavor:       flavor,
	}, nil
}

// DetectFlavor infers whether client talks to a genuine MongoDB server or to
// Amazon DocumentDB. It returns the flavor plus a human-readable description
// of the evidence, for logging.
//
// The signal is gossiped cluster time. Every MongoDB replica set and sharded
// cluster has returned $clusterTime in every response since 3.6. DocumentDB
// has no cluster-time gossip at all, because it doesn’t implement causal
// consistency, so a server that advertises replica-set membership yet omits
// $clusterTime is DocumentDB.
//
// The replica-set advertisement is a necessary part of the test: a genuine
// standalone mongod also omits $clusterTime, and without that check we’d
// misreport standalones as DocumentDB.
//
// This is a heuristic against a server that deliberately imitates MongoDB, so
// it can be overridden from the command line.
func DetectFlavor(ctx context.Context, client *mongo.Client) (Flavor, string, error) {
	raw, err := GetHelloRaw(ctx, client, option.None[*readpref.ReadPref]())
	if err != nil {
		return "", "", errors.Wrap(err, "failed to run hello")
	}

	return flavorFromHello(raw)
}

// flavorFromHello holds DetectFlavor’s logic, split out so that it’s testable
// against synthetic hello responses.
func flavorFromHello(raw bson.Raw) (Flavor, string, error) {
	// A standalone mongod legitimately lacks $clusterTime, so we can only
	// draw a conclusion for servers that claim replica-set membership.
	hasMe, err := mbson.RawContains(raw, "me")
	if err != nil {
		return "", "", errors.Wrapf(err, "failed to check for %#q in hello response (%v)", "me", raw)
	}

	if !hasMe {
		return FlavorMongoDB, "standalone server; assuming MongoDB", nil
	}

	hasClusterTime, err := mbson.RawContains(raw, "$clusterTime")
	if err != nil {
		return "", "", errors.Wrapf(err, "failed to check for %#q in hello response (%v)", "$clusterTime", raw)
	}

	if hasClusterTime {
		return FlavorMongoDB, "hello response gossips $clusterTime", nil
	}

	return FlavorDocumentDB, "hello response advertises a replica set but omits $clusterTime", nil
}

func getTopology(ctx context.Context, client *mongo.Client) (ClusterTopology, error) {
	// The topology won’t vary amongst the nodes.
	raw, err := GetHelloRaw(ctx, client, option.None[*readpref.ReadPref]())
	if err != nil {
		return "", errors.Wrapf(err, "failed learn topology")
	}

	hasMsg, err := mbson.RawContains(raw, "msg")
	if err != nil {
		return "", errors.Wrapf(err, "failed to check for %#q in hello response (%v)", "msg", raw)
	}

	if hasMsg {
		return TopologySharded, nil
	}

	hasMe, err := mbson.RawContains(raw, "me")
	if err != nil {
		return "", errors.Wrapf(err, "failed to check for %#q in hello response (%v)", "me", raw)
	}

	return lo.Ternary(hasMe, TopologyReplset, TopologyStandalone), nil
}

// GetHelloRaw returns the result of a `hello` (or, if needed,
// `isMaster`) command.
func GetHelloRaw(
	ctx context.Context,
	client *mongo.Client,
	readPref option.Option[*readpref.ReadPref],
) (bson.Raw, error) {
	opts := options.RunCmd()
	if rp, has := readPref.Get(); has {
		opts = opts.SetReadPreference(rp)
	}

	resp := client.Database("admin").RunCommand(
		ctx,
		bson.D{{"hello", 1}},
		opts,
	)

	if resp.Err() != nil {
		resp = client.Database("admin").RunCommand(
			ctx,
			bson.D{{"isMaster", 1}},
			opts,
		)
	}

	raw, err := resp.Raw()

	// Proactively check for the problem that
	// https://jira.mongodb.org/browse/SERVER-52654 fixed.
	//
	// We check for “me” to avoid failing if the cluster is a standalone,
	// in which case the hello response legitimately lacks an operationTime.
	if err == nil && !raw.Lookup("me").IsZero() {
		const opTimeName = "operationTime"
		_, err := raw.LookupErr(opTimeName)
		if err != nil {
			return nil, fmt.Errorf("server response lacks %#q; force an election, then retry", opTimeName)
		}
	}

	return resp.Raw()
}
