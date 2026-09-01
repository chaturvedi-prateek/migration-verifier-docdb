package verifier

import (
	"context"
	"fmt"
	"strings"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mmongo"
	mapset "github.com/deckarep/golang-set/v2"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// DocumentDB enables change streams per collection, per database, or
// cluster-wide, and they are disabled by default. A change stream opened
// against a namespace with change streams disabled yields no events for that
// namespace, which for the verifier would mean silently missing rechecks and
// reporting a false match. So we verify coverage before doing any work.
//
// See “Enabling change streams” in the Amazon DocumentDB developer guide.
const (
	// enableAllChangeStreamsCmd is the remediation we suggest to users.
	enableAllChangeStreamsCmd = `db.adminCommand({modifyChangeStreams: 1, database: "", collection: "", enable: true})`

	// docDBRetentionAdvisoryHours is the retention below which we consider a
	// verification run to be at meaningful risk of a resume-token gap.
	docDBRetentionAdvisoryHours = 24
)

// changeStreamScope is one entry of $listChangeStreams output. An empty
// string is a wildcard, so {database: "", collection: ""} means the whole
// cluster and {database: "bar", collection: ""} means all of “bar”.
type changeStreamScope struct {
	Database   string `bson:"database"`
	Collection string `bson:"collection"`
}

func (s changeStreamScope) String() string {
	switch {
	case s.Database == "":
		return "<all databases>"
	case s.Collection == "":
		return s.Database + ".<all collections>"
	default:
		return s.Database + "." + s.Collection
	}
}

// covers reports whether this scope enables change streams for the given
// namespace. Per the DocumentDB docs, a collection has change streams enabled
// if the collection is explicitly enabled, its database is enabled, or all
// databases are enabled.
func (s changeStreamScope) covers(db, coll string) bool {
	if s.Database == "" {
		return true
	}

	if s.Database != db {
		return false
	}

	return s.Collection == "" || s.Collection == coll
}

// isClusterWide reports whether this scope covers every namespace, including
// ones not yet created.
func (s changeStreamScope) isClusterWide() bool {
	return s.Database == "" && s.Collection == ""
}

// listEnabledChangeStreams returns the scopes for which the DocumentDB cluster
// has change streams enabled.
func listEnabledChangeStreams(
	ctx context.Context,
	client *mongo.Client,
) ([]changeStreamScope, error) {
	// $listChangeStreams is only valid as a cluster-level aggregation, so we
	// run it as a raw command rather than via a collection handle.
	cursor, err := client.Database("admin").Aggregate(
		ctx,
		mongo.Pipeline{{{"$listChangeStreams", 1}}},
	)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to run %#q", "$listChangeStreams")
	}

	var scopes []changeStreamScope
	if err := cursor.All(ctx, &scopes); err != nil {
		return nil, errors.Wrapf(err, "failed to read %#q output", "$listChangeStreams")
	}

	return scopes, nil
}

// checkDocumentDBChangeStreamsEnabled fails unless the source cluster has
// change streams enabled for everything the verifier intends to watch.
//
// When namespaces is empty the verifier watches the whole cluster, including
// collections that don’t exist yet, so we require the cluster-wide wildcard.
// Enabling each database individually would leave a newly created database
// unwatched, and the verifier would not notice.
func checkDocumentDBChangeStreamsEnabled(
	ctx context.Context,
	logger *logger.Logger,
	client *mongo.Client,
	namespaces []string,
) error {
	scopes, err := listEnabledChangeStreams(ctx, client)
	if err != nil {
		return err
	}

	logger.Debug().
		Int("scopeCount", len(scopes)).
		Msg("Read source’s enabled change streams.")

	if len(namespaces) == 0 {
		if anyScopeIsClusterWide(scopes) {
			logger.Info().
				Msg("Source has change streams enabled cluster-wide.")

			return nil
		}

		return errors.Errorf(
			"source is DocumentDB, which disables change streams by default, "+
				"and this run verifies all namespaces, so change streams must be "+
				"enabled cluster-wide (currently enabled for: %s). "+
				"Enable them with: %s",
			describeScopes(scopes),
			enableAllChangeStreamsCmd,
		)
	}

	uncovered := findUncoveredNamespaces(scopes, namespaces)

	if len(uncovered) > 0 {
		return errors.Errorf(
			"source is DocumentDB, which disables change streams by default, "+
				"and these verified namespaces lack them: %s (currently enabled for: %s). "+
				"Without change streams the verifier cannot detect concurrent writes "+
				"and may report a false match. Enable them per namespace with "+
				`db.adminCommand({modifyChangeStreams: 1, database: "<db>", collection: "<coll>", enable: true}), `+
				"or cluster-wide with: %s",
			strings.Join(uncovered, ", "),
			describeScopes(scopes),
			enableAllChangeStreamsCmd,
		)
	}

	logger.Info().
		Int("namespaceCount", len(namespaces)).
		Msg("Source has change streams enabled for all verified namespaces.")

	return nil
}

// anyScopeIsClusterWide reports whether the scopes enable change streams for
// every namespace on the cluster, including ones not yet created.
func anyScopeIsClusterWide(scopes []changeStreamScope) bool {
	for _, scope := range scopes {
		if scope.isClusterWide() {
			return true
		}
	}

	return false
}

// findUncoveredNamespaces returns those namespaces that no scope enables
// change streams for, in the order given.
func findUncoveredNamespaces(
	scopes []changeStreamScope,
	namespaces []string,
) []string {
	var uncovered []string

	for _, ns := range namespaces {
		db, coll := mmongo.SplitNamespace(ns)

		covered := false
		for _, scope := range scopes {
			if scope.covers(db, coll) {
				covered = true
				break
			}
		}

		if !covered {
			uncovered = append(uncovered, ns)
		}
	}

	return uncovered
}

func describeScopes(scopes []changeStreamScope) string {
	if len(scopes) == 0 {
		return "nothing"
	}

	strs := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		strs = append(strs, scope.String())
	}

	return strings.Join(strs, ", ")
}

// warnDocumentDBChangeStreamRetention advises the user about change stream log
// retention.
//
// DocumentDB retains change stream events for 3 hours by default (max 7 days).
// If the verifier is interrupted for longer than the retention window, its
// persisted resume token expires, and resuming would leave an unobserved gap
// in the change stream — which would silently invalidate the verification.
//
// The retention setting lives in a DB cluster parameter group and isn’t
// readable over the MongoDB wire protocol, so we can only advise, not check.
func warnDocumentDBChangeStreamRetention(logger *logger.Logger) {
	logger.Warn().
		Int("recommendedHours", docDBRetentionAdvisoryHours).
		Msg("Source is DocumentDB, which retains change stream events for only 3 hours by default. " +
			"If this verification is interrupted for longer than the retention window, its resume token " +
			"will expire and the run must restart from the beginning. Consider raising " +
			"change_stream_log_retention_duration on the cluster parameter group.")
}

// docDBUnsupported builds the error we return when a requested option can’t
// work against DocumentDB.
func docDBUnsupported(what, why, instead string) error {
	return fmt.Errorf(
		"%s is not supported when the source is DocumentDB, because %s; use %s instead",
		what,
		why,
		instead,
	)
}

// checkDocumentDBReadPreference rejects read preferences that would break the
// verifier’s correctness argument on DocumentDB.
//
// Without gossiped cluster time the verifier can’t pin reads with
// afterClusterTime, so it instead relies on DocumentDB’s guarantee that reads
// from the primary are read-after-write consistent. A secondary read is only
// eventually consistent, which would let a scan observe a state older than the
// change stream’s start point — precisely the gap that rechecks are meant to
// close.
func checkDocumentDBReadPreference(mode string) error {
	switch mode {
	case "primary", "primaryPreferred":
		return nil
	}

	return errors.Errorf(
		"read preference %#q is not supported when the source is DocumentDB: "+
			"reads from DocumentDB replicas are only eventually consistent, which would let "+
			"the verifier read data older than its change stream and report a false match; "+
			"use %#q",
		mode,
		"primary",
	)
}

// runDocumentDBSourcePreflight runs every DocumentDB-specific source check
// that needs a live connection. It must run after the source namespaces are
// known.
func (verifier *Verifier) runDocumentDBSourcePreflight(ctx context.Context) error {
	if verifier.srcClusterInfo == nil || !verifier.srcClusterInfo.Flavor.IsDocumentDB() {
		return nil
	}

	warnDocumentDBChangeStreamRetention(verifier.logger)

	return checkDocumentDBChangeStreamsEnabled(
		ctx,
		verifier.logger,
		verifier.srcClient,
		verifier.srcNamespaces,
	)
}

// srcIsDocumentDB reports whether the source cluster is DocumentDB. It is safe
// to call before the source URI is set, in which case it reports false.
func (verifier *Verifier) srcIsDocumentDB() bool {
	return verifier.srcClusterInfo != nil && verifier.srcClusterInfo.Flavor.IsDocumentDB()
}

// docDBIncomparableCollectionOptions are collection options that cannot be
// compared meaningfully between DocumentDB and MongoDB.
var docDBIncomparableCollectionOptions = mapset.NewSet(
	// Storage-engine configuration. DocumentDB reports
	// storageEngine.documentDB.compression; MongoDB reports wiredTiger
	// settings or nothing at all. The two describe different engines and can
	// never agree.
	"storageEngine",

	// Long removed from MongoDB, still reported by DocumentDB.
	"autoIndexId",
)

// normalizeCollectionOptionsForDocumentDB strips collection options that
// cannot be compared across implementations, so that metadata comparison
// reflects real differences rather than engine trivia.
//
// It also drops `capped: false`, which DocumentDB states explicitly and
// MongoDB omits. That one is not cosmetic: an unequal `capped` value makes
// compareCollectionSpecifications report the collection uncomparable, which
// skips document verification for the namespace entirely.
func normalizeCollectionOptionsForDocumentDB(opts bson.Raw) (bson.Raw, error) {
	if len(opts) == 0 {
		return opts, nil
	}

	elems, err := opts.Elements()
	if err != nil {
		return nil, errors.Wrapf(err, "reading collection options (%v)", opts)
	}

	kept := bson.D{}

	for _, el := range elems {
		key := el.Key()

		if docDBIncomparableCollectionOptions.Contains(key) {
			continue
		}

		// `capped: false` is equivalent to the field being absent.
		if key == "capped" {
			asBool, ok := el.Value().BooleanOK()
			if ok && !asBool {
				continue
			}
		}

		kept = append(kept, bson.E{Key: key, Value: el.Value()})
	}

	raw, err := bson.Marshal(kept)
	if err != nil {
		return nil, errors.Wrapf(err, "re-encoding collection options (%v)", kept)
	}

	return raw, nil
}

// isCappedOption reports whether a collection options document marks the
// collection capped. A missing field means not capped.
func isCappedOption(opts bson.Raw) bool {
	asBool, ok := opts.Lookup("capped").BooleanOK()

	return ok && asBool
}

// docDBIncomparableIndexFields are index-specification fields that describe
// engine internals rather than the index's meaning, and so cannot be compared
// between DocumentDB and MongoDB.
//
// Observed on DocumentDB 5.0.0 against MongoDB 7.0:
//
//	field                  DocumentDB   MongoDB
//	v                      4            2
//	ns                     present      absent (removed in MongoDB 4.4)
//	2dsphereIndexVersion   1            3
//
// Everything that defines the index — key, unique, sparse,
// partialFilterExpression, expireAfterSeconds — matches exactly, so dropping
// these three fields compares the indexes that actually exist rather than the
// engines that hold them.
//
// The `v` mismatch is not merely noisy: the index comparator rejects
// DocumentDB's value outright with "index has an unexpected `v` value: 4",
// which aborts the whole verification. `v` is rewritten rather than dropped,
// because the comparator also requires the field to be present.
var docDBIncomparableIndexFields = mapset.NewSet(
	"ns",
	"2dsphereIndexVersion",
)

// docDBNormalizedIndexVersion is the `v` value we rewrite index specs to.
//
// `v` cannot simply be dropped: the index comparator requires it and fails
// with "extracting `v` from index spec: element not found". So we rewrite both
// sides to MongoDB's modern value, which makes the comparison meaningful
// without asking the comparator to understand DocumentDB's numbering.
const docDBNormalizedIndexVersion = int32(2)

// normalizeIndexSpecForDocumentDB strips engine-internal fields from an index
// specification so that source and destination can be compared meaningfully.
func normalizeIndexSpecForDocumentDB(spec bson.Raw) (bson.Raw, error) {
	if len(spec) == 0 {
		return spec, nil
	}

	elems, err := spec.Elements()
	if err != nil {
		return nil, errors.Wrapf(err, "reading index specification (%v)", spec)
	}

	kept := bson.D{}

	for _, el := range elems {
		key := el.Key()

		if docDBIncomparableIndexFields.Contains(key) {
			continue
		}

		if key == "v" {
			kept = append(kept, bson.E{Key: key, Value: docDBNormalizedIndexVersion})
			continue
		}

		kept = append(kept, bson.E{Key: key, Value: el.Value()})
	}

	raw, err := bson.Marshal(kept)
	if err != nil {
		return nil, errors.Wrapf(err, "re-encoding index specification (%v)", kept)
	}

	return raw, nil
}

// normalizeIndexSpecsForDocumentDB normalizes every spec in the map in place.
func normalizeIndexSpecsForDocumentDB(specs map[string]bson.Raw) error {
	for name, spec := range specs {
		normalized, err := normalizeIndexSpecForDocumentDB(spec)
		if err != nil {
			return errors.Wrapf(err, "normalizing index %#q", name)
		}

		specs[name] = normalized
	}

	return nil
}

// sessionOptsForFlavor returns the session options to use against a cluster of
// the given flavor.
//
// The Go driver creates causally-consistent sessions by default. DocumentDB
// does not implement causal consistency and rejects such reads outright with
// error 303, "Feature not supported: 'causal consistency'".
//
// This does not fail immediately, which is what makes it dangerous: a session's
// first operation carries no afterClusterTime, so opening a change stream
// succeeds. Only once the session has recorded an operationTime does the driver
// start sending afterClusterTime — so the failure appears on the first *resume*,
// which in practice means during a failover, long after the run looked healthy.
//
// Disabling causal consistency costs us nothing here. The verifier never relied
// on it against DocumentDB: reads are ordered by opening the change reader
// before any scan and trusting the primary's read-after-write guarantee.
func sessionOptsForFlavor(flavor util.Flavor) *options.SessionOptionsBuilder {
	opts := options.Session()

	if flavor.IsDocumentDB() {
		opts = opts.SetCausalConsistency(false)
	}

	return opts
}
