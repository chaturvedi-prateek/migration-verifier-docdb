package verifier

import (
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
}

func newReadProgress() *readProgress {
	return &readProgress{last: msync.NewTypedAtomic(time.Now())}
}

func (p *readProgress) touch() {
	p.last.Store(time.Now())
}

func (p *readProgress) lastProgress() time.Time {
	return p.last.Load()
}

// progressNotifier records reader progress on every success notification
// before forwarding the notification to the wrapped notifier.
type progressNotifier struct {
	retry.SuccessNotifier
	progress *readProgress
}

var _ retry.SuccessNotifier = progressNotifier{}

func (n progressNotifier) NoteSuccess(tmpl string, args ...any) {
	n.progress.touch()
	n.SuccessNotifier.NoteSuccess(tmpl, args...)
}
