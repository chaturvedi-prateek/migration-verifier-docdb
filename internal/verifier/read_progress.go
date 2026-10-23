package verifier

import (
	"sync/atomic"
	"time"

	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/msync"
	"github.com/pkg/errors"
)

// errReaderStalled indicates that a document reader made no progress for
// longer than the comparator tolerates.
var errReaderStalled = errors.New("document reader stalled")

// readProgress records when a document reader last made progress, i.e.,
// received a document from the server or handed documents to the comparator.
//
// The comparator uses this to tell a reader that is slow (e.g., a large range
// scan on a busy cluster) from one that is stuck.
type readProgress struct {
	last *msync.TypedAtomic[time.Time]

	// A natural-partition destination reader needs IDs from the source
	// before it can query the destination. Only that wait depends on source
	// progress; once the IDs arrive, destination query progress stands alone.
	inputProgress   *readProgress
	waitingForInput atomic.Bool
}

func newReadProgress() *readProgress {
	return &readProgress{last: msync.NewTypedAtomic(time.Now())}
}

func (p *readProgress) touch() {
	p.last.Store(time.Now())
}

func (p *readProgress) lastProgress() time.Time {
	// Read the phase before the timestamp: setWaitingForInput records a new
	// timestamp before publishing the phase change.
	waiting := p.waitingForInput.Load()
	last := p.last.Load()
	if p.inputProgress != nil && waiting {
		if inputLast := p.inputProgress.lastProgress(); inputLast.After(last) {
			last = inputLast
		}
	}
	return last
}

// inputWaitNotifier is implemented by notifiers that distinguish waiting
// for source IDs from running a destination query.
type inputWaitNotifier interface {
	setWaitingForInput(bool)
}

// progressNotifier records reader progress on every success notification
// before forwarding the notification to the wrapped notifier.
type progressNotifier struct {
	retry.SuccessNotifier
	progress *readProgress
}

var _ retry.SuccessNotifier = progressNotifier{}
var _ inputWaitNotifier = progressNotifier{}

func (n progressNotifier) setWaitingForInput(waiting bool) {
	// Changing phases starts a new wait. Record it before disabling the
	// dependency so the watchdog cannot see an old destination timestamp
	// between receiving source IDs and opening the destination query.
	n.progress.touch()
	n.progress.waitingForInput.Store(waiting)
}

func (n progressNotifier) NoteSuccess(tmpl string, args ...any) {
	n.progress.touch()
	n.SuccessNotifier.NoteSuccess(tmpl, args...)
}
