package verifier

import (
	"context"
	"testing"
	"time"

	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/testutil"
	"github.com/10gen/migration-verifier/internal/verifier/compare"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func newTestComparator(
	t *testing.T,
	srcProgress, dstProgress *readProgress,
	readTimeout time.Duration,
) *comparator {
	verifier := NewVerifier(VerifierSettings{}, "stderr")
	verifier.docCompareMethod = compare.Binary

	task := &tasks.Task{
		PrimaryKey: bson.NewObjectID(),
		QueryFilter: tasks.QueryFilter{
			Namespace: "testdb.testns",
		},
	}

	c := newComparator(
		verifier,
		0,
		&testutil.MockSuccessNotifier{T: t},
		task,
		srcProgress,
		dstProgress,
	)
	c.readTimeout = readTimeout
	c.stallCheckInterval = readTimeout / 10

	return c
}

func docMsg(docs ...bson.D) compare.ToComparatorMsg {
	var msg compare.ToComparatorMsg

	for i, doc := range docs {
		msg.DocsWithTS = append(
			msg.DocsWithTS,
			compare.NewDocWithTSFromPool(
				testutil.MustMarshal(doc),
				bson.Timestamp{T: 1, I: uint32(i)},
			),
		)
	}

	return msg
}

// runComparator runs the comparator and fails the test if it doesn’t
// return in time. The comparator must never block indefinitely.
func runComparator(
	t *testing.T,
	c *comparator,
	srcChannel, dstChannel <-chan compare.ToComparatorMsg,
) error {
	reportsChan := make(chan DocCompareReport, 100)
	errChan := make(chan error, 1)

	go func() {
		errChan <- c.run(t.Context(), srcChannel, dstChannel, reportsChan)
	}()

	select {
	case err := <-errChan:
		return err
	case <-time.After(10 * time.Second):
		require.FailNow(t, "comparator did not return")
		return nil
	}
}

// A reader that makes no progress must cause the comparator to return an
// error rather than wait forever.
func TestComparatorFailsOnStalledReader(t *testing.T) {
	srcProgress := newReadProgress()
	dstProgress := newReadProgress()
	c := newTestComparator(t, srcProgress, dstProgress, 200*time.Millisecond)

	dstChannel := make(chan compare.ToComparatorMsg, 1)
	dstChannel <- docMsg(bson.D{{"_id", 1}})
	close(dstChannel)

	// The source reader never sends anything and never reports progress.
	srcChannel := make(chan compare.ToComparatorMsg)

	err := runComparator(t, c, srcChannel, dstChannel)
	require.ErrorIs(t, err, errReaderStalled)
	require.ErrorContains(t, err, "source")
}

// A reader that is slow but keeps reporting progress must not be treated as
// stalled, even when it takes longer than the read timeout to send a batch.
func TestComparatorToleratesSlowReader(t *testing.T) {
	srcProgress := newReadProgress()
	dstProgress := newReadProgress()
	readTimeout := 200 * time.Millisecond
	c := newTestComparator(t, srcProgress, dstProgress, readTimeout)

	srcChannel := make(chan compare.ToComparatorMsg)
	dstChannel := make(chan compare.ToComparatorMsg, 1)
	dstChannel <- docMsg(bson.D{{"_id", 1}})
	close(dstChannel)

	go func() {
		defer close(srcChannel)

		// Simulate a reader that receives documents from the server
		// slowly, so its first batch takes several read timeouts.
		deadline := time.Now().Add(5 * readTimeout)
		for time.Now().Before(deadline) {
			srcProgress.touch()
			time.Sleep(readTimeout / 4)
		}

		select {
		case srcChannel <- docMsg(bson.D{{"_id", 1}}):
		case <-time.After(10 * time.Second):
		}
	}()

	err := runComparator(t, c, srcChannel, dstChannel)
	require.NoError(t, err)
}

// Time that the comparator spends not listening (e.g., comparing documents
// or persisting results) must not count against a reader.
func TestComparatorIgnoresTimeNotListening(t *testing.T) {
	srcProgress := newReadProgress()
	dstProgress := newReadProgress()
	readTimeout := 200 * time.Millisecond
	c := newTestComparator(t, srcProgress, dstProgress, readTimeout)

	// Both readers last reported progress long ago, as if blocked on a
	// comparator that was busy elsewhere.
	srcProgress.last.Store(time.Now().Add(-time.Hour))
	dstProgress.last.Store(time.Now().Add(-time.Hour))

	srcChannel := make(chan compare.ToComparatorMsg, 1)
	srcChannel <- docMsg(bson.D{{"_id", 1}})
	close(srcChannel)

	dstChannel := make(chan compare.ToComparatorMsg, 1)
	dstChannel <- docMsg(bson.D{{"_id", 1}})
	close(dstChannel)

	err := runComparator(t, c, srcChannel, dstChannel)
	require.NoError(t, err)
}

// The progress notifier must record progress and forward notifications.
func TestProgressNotifier(t *testing.T) {
	progress := newReadProgress()
	progress.last.Store(time.Now().Add(-time.Hour))

	inner := &testutil.MockSuccessNotifier{}
	notifier := progressNotifier{inner, progress}

	before := time.Now()
	notifier.NoteSuccess("received %d documents", 3)

	require.False(t, progress.lastProgress().Before(before), "progress should be recorded")
	require.Equal(t, []string{"received 3 documents"}, inner.Messages())
}

// A canceled context must stop the comparator promptly.
func TestComparatorStopsOnCancellation(t *testing.T) {
	c := newTestComparator(t, newReadProgress(), newReadProgress(), time.Hour)

	ctx, cancel := contextplus.WithCancelCause(t.Context())
	cancel(context.Canceled)

	errChan := make(chan error, 1)
	go func() {
		errChan <- c.run(
			ctx,
			make(chan compare.ToComparatorMsg),
			make(chan compare.ToComparatorMsg),
			make(chan DocCompareReport, 1),
		)
	}()

	select {
	case err := <-errChan:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "comparator did not return after cancellation")
	}
}

// Waiting for source IDs follows source liveness, but running a destination
// query must not borrow source progress.
func TestDestinationProgressDependsOnSourceOnlyWhileWaitingForIDs(t *testing.T) {
	srcProgress := newReadProgress()
	dstProgress := newReadProgress()
	dstProgress.inputProgress = srcProgress
	notifier := progressNotifier{&testutil.MockSuccessNotifier{}, dstProgress}
	c := newTestComparator(t, srcProgress, dstProgress, time.Minute)
	old := time.Now().Add(-time.Hour)
	dstProgress.last.Store(old)

	notifier.setWaitingForInput(true)
	require.NoError(t, c.checkReaderStalled("destination", dstProgress, old))

	notifier.setWaitingForInput(false)
	dstProgress.last.Store(old)
	require.ErrorIs(t, c.checkReaderStalled("destination", dstProgress, old), errReaderStalled)

	notifier.setWaitingForInput(true)
	dstProgress.last.Store(old)
	srcProgress.last.Store(old)
	require.ErrorIs(t, c.checkReaderStalled("destination", dstProgress, old), errReaderStalled)
}

func TestComparatorWaitsForSourceIDsWhileSourceIsActive(t *testing.T) {
	srcProgress := newReadProgress()
	dstProgress := newReadProgress()
	dstProgress.inputProgress = srcProgress
	notifier := progressNotifier{&testutil.MockSuccessNotifier{}, dstProgress}
	notifier.setWaitingForInput(true)
	c := newTestComparator(t, srcProgress, dstProgress, 200*time.Millisecond)
	srcChannel := make(chan compare.ToComparatorMsg)
	dstChannel := make(chan compare.ToComparatorMsg)
	done := make(chan struct{})

	go func() {
		defer close(done)
		defer close(srcChannel)
		defer close(dstChannel)
		ticker := time.NewTicker(c.readTimeout / 4)
		defer ticker.Stop()
		ready := time.NewTimer(3 * c.readTimeout)
		defer ready.Stop()
		for {
			select {
			case <-t.Context().Done():
				return
			case <-ticker.C:
				srcProgress.touch()
			case <-ready.C:
				// The empty batch represents the first completed source read;
				// no data has to be pooled for this channel-liveness test.
				select {
				case srcChannel <- compare.ToComparatorMsg{}:
				case <-t.Context().Done():
					return
				}
				notifier.NoteSuccess("received source IDs")
				notifier.setWaitingForInput(false)
				select {
				case dstChannel <- compare.ToComparatorMsg{}:
				case <-t.Context().Done():
				}
				return
			}
		}
	}()

	require.NoError(t, runComparator(t, c, srcChannel, dstChannel))
	<-done
}

func TestComparatorFailsOnDestinationOrBothReadersStalling(t *testing.T) {
	for _, both := range []bool{false, true} {
		name := "destination"
		if both {
			name = "both"
		}
		t.Run(name, func(t *testing.T) {
			c := newTestComparator(t, newReadProgress(), newReadProgress(), 200*time.Millisecond)
			srcChannel := make(chan compare.ToComparatorMsg, 1)
			if !both {
				srcChannel <- compare.ToComparatorMsg{}
				close(srcChannel)
			}
			err := runComparator(t, c, srcChannel, make(chan compare.ToComparatorMsg))
			require.ErrorIs(t, err, errReaderStalled)
			if !both {
				require.ErrorContains(t, err, "destination")
			}
		})
	}
}
