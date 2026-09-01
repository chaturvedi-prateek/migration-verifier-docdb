package util

import (
	"github.com/10gen/migration-verifier/mbson"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func GetClusterTimeFromSession(sess *mongo.Session) (bson.Timestamp, error) {
	clusterTimeRaw := sess.ClusterTime()

	if clusterTimeRaw == nil {
		panic("found empty session cluster time but need nonempty")
	}

	ctrv, err := clusterTimeRaw.LookupErr("$clusterTime", "clusterTime")
	if err != nil {
		return bson.Timestamp{}, errors.Wrapf(err, "finding clusterTime in session cluster time document (%v)", clusterTimeRaw)
	}

	return mbson.CastRawValue[bson.Timestamp](ctrv)
}

// GetSessionTimestamp returns a timestamp describing how current the session’s
// view of the server is.
//
// For MongoDB this is the gossiped $clusterTime. DocumentDB gossips no cluster
// time at all — it does not implement causal consistency — so there we use
// operationTime, which the server does return in command responses. The two
// are not interchangeable in general, but both serve the purpose we need here:
// a monotonic marker of how far the server has advanced.
func GetSessionTimestamp(sess *mongo.Session, flavor Flavor) (bson.Timestamp, error) {
	if !flavor.IsDocumentDB() {
		return GetClusterTimeFromSession(sess)
	}

	opTime := sess.OperationTime()
	if opTime == nil {
		return bson.Timestamp{}, errors.New(
			"session has no operationTime; DocumentDB is expected to return one in command responses",
		)
	}

	return *opTime, nil
}
