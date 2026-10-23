package verifier

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/verifier/compare"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Exercise the complete fetch/retry/compare pipeline. The command monitor
// holds a find until its operation context is canceled, simulating a reader
// waiting on a server without overriding any verifier callbacks.
func TestFetchAndCompareStopsReaders(t *testing.T) {
	uri := os.Getenv("MVTEST_SRC")
	if uri == "" {
		t.Skip("requires MVTEST_SRC")
	}
	setup, err := mongo.Connect(options.Client().ApplyURI(uri))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, setup.Disconnect(context.WithoutCancel(t.Context()))) })
	for _, tc := range []struct {
		name               string
		blockSrc, blockDst bool
		cancel             bool
	}{
		{name: "source", blockSrc: true},
		{name: "destination", blockDst: true},
		{name: "both", blockSrc: true, blockDst: true},
		{name: "upstream cancellation", blockSrc: true, blockDst: true, cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setup.Database("__mv_reader_shutdown_" + bson.NewObjectID().Hex())
			t.Cleanup(func() { require.NoError(t, db.Drop(context.WithoutCancel(t.Context()))) })
			_, err := db.Collection("docs").InsertOne(t.Context(), bson.D{{"_id", 1}})
			require.NoError(t, err)
			entered := make(chan struct{}, 2)
			exited := make(chan struct{}, 2)
			newClient := func(block bool) *mongo.Client {
				opts := options.Client().ApplyURI(uri)
				if block {
					opts.SetMonitor(&event.CommandMonitor{Started: func(ctx context.Context, e *event.CommandStartedEvent) {
						if e.CommandName == "find" {
							entered <- struct{}{}
							<-ctx.Done()
							exited <- struct{}{}
						}
					}})
				}
				client, err := mongo.Connect(opts)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, client.Disconnect(context.WithoutCancel(t.Context()))) })
				return client
			}
			v := NewVerifier(VerifierSettings{}, "stderr")
			v.srcClient, v.dstClient = newClient(tc.blockSrc), newClient(tc.blockDst)
			info, err := util.GetClusterInfo(t.Context(), v.logger, setup)
			require.NoError(t, err)
			v.srcClusterInfo, v.dstClusterInfo = &info, &info
			start := bson.Timestamp{T: 1, I: 1}
			srcCommon, dstCommon := newChangeReaderCommon(src), newChangeReaderCommon(dst)
			srcCommon.startAtTS, dstCommon.startAtTS = &start, &start
			v.srcChangeReader = &ChangeStreamReader{ChangeReaderCommon: srcCommon}
			v.dstChangeReader = &ChangeStreamReader{ChangeReaderCommon: dstCommon}
			v.docCompareMethod = compare.Binary
			v.documentReadTimeout = 300 * time.Millisecond
			ctx, cancel := contextplus.WithCancelCause(t.Context())
			defer cancel(context.Canceled)
			task := &tasks.Task{
				PrimaryKey:  bson.NewObjectID(),
				Ids:         []bson.RawValue{bsontools.ToRawValue(int32(1))},
				QueryFilter: tasks.QueryFilter{Namespace: db.Name() + ".docs"},
			}
			results := v.FetchAndCompareDocuments(ctx, 0, task)
			blocked := 0
			for _, b := range []bool{tc.blockSrc, tc.blockDst} {
				if b {
					blocked++
					select {
					case <-entered:
					case <-time.After(5 * time.Second):
						require.FailNow(t, "reader did not start")
					}
				}
			}
			if tc.cancel {
				cancel(context.Canceled)
			}
			select {
			case result, open := <-results:
				if tc.cancel {
					if open {
						_, err := result.Get()
						require.ErrorIs(t, err, context.Canceled)
					}
				} else {
					require.Equal(t, true, open)
					_, err := result.Get()
					require.ErrorIs(t, err, errReaderStalled)
				}
			case <-time.After(5 * time.Second):
				require.FailNow(t, "fetch/compare did not stop")
			}
			for range blocked {
				select {
				case <-exited:
				case <-time.After(5 * time.Second):
					require.FailNow(t, "reader was not canceled")
				}
			}
			select {
			case _, open := <-results:
				require.Equal(t, false, open, "result channel must close")
			case <-time.After(5 * time.Second):
				require.FailNow(t, "result channel did not close")
			}
		})
	}
}
