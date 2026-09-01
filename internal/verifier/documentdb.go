package verifier

import (
	"context"
	"fmt"
	"strings"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/mmongo"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/mongo"
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
