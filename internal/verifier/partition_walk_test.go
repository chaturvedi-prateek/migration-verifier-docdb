package verifier

import (
	"math"
	"testing"

	"github.com/10gen/migration-verifier/internal/types"
	"github.com/stretchr/testify/assert"
)

// TestDocsPerPartitionFor checks the document-count target that drives how far
// the _id-index walk skips between partition boundaries.
func TestDocsPerPartitionFor(t *testing.T) {
	const partitionSize = types.ByteCount(400 * 1024 * 1024)

	for _, curTest := range []struct {
		name       string
		collBytes  types.ByteCount
		docsCount  types.DocumentCount
		partSize   types.ByteCount
		expected   int64
		singlePart bool
	}{
		{
			name:       "collection smaller than one partition is not split",
			collBytes:  partitionSize / 2,
			docsCount:  1_000,
			partSize:   partitionSize,
			singlePart: true,
		},
		{
			name:       "empty collection is not split",
			collBytes:  0,
			docsCount:  0,
			partSize:   partitionSize,
			singlePart: true,
		},
		{
			// 4 GiB of 1 KiB documents at 400 MiB partitions is ~10
			// partitions over ~4.2M docs.
			name:      "splits evenly by document count",
			collBytes: types.ByteCount(4 * 1024 * 1024 * 1024),
			docsCount: types.DocumentCount(4 * 1024 * 1024),
			partSize:  partitionSize,
			expected:  409_600,
		},
		{
			// Documents far larger than average still yield at least one doc
			// per partition rather than zero, which would loop forever.
			name:      "never returns less than one document per partition",
			collBytes: types.ByteCount(100 * 1024 * 1024 * 1024),
			docsCount: 10,
			partSize:  partitionSize,
			expected:  1,
		},
	} {
		t.Run(curTest.name, func(t *testing.T) {
			actual := docsPerPartitionFor(
				curTest.collBytes,
				curTest.docsCount,
				curTest.partSize,
			)

			if curTest.singlePart {
				assert.Equal(
					t,
					int64(math.MaxInt64),
					actual,
					"collection should fit in a single partition",
				)

				return
			}

			assert.Equal(t, curTest.expected, actual)
			assert.Positive(t, actual, "must make forward progress")
		})
	}
}

// TestDocsPerPartitionFor_TerabyteScale documents the partition counts the
// walk produces at the sizes this strategy exists to handle. The walk’s cost
// is one pass over the _id index regardless, but the partition count drives
// how many boundary queries and task documents we create.
func TestDocsPerPartitionFor_TerabyteScale(t *testing.T) {
	const (
		tebibyte      = types.ByteCount(1024 * 1024 * 1024 * 1024)
		partitionSize = types.ByteCount(400 * 1024 * 1024)
		kibibyte      = 1024
	)

	for _, curTest := range []struct {
		name                  string
		collBytes             types.ByteCount
		expectedPartitionsMin int64
		expectedPartitionsMax int64
	}{
		{
			name:                  "1 TiB",
			collBytes:             tebibyte,
			expectedPartitionsMin: 2_500,
			expectedPartitionsMax: 2_700,
		},
		{
			name:                  "10 TiB",
			collBytes:             10 * tebibyte,
			expectedPartitionsMin: 25_000,
			expectedPartitionsMax: 27_000,
		},
	} {
		t.Run(curTest.name, func(t *testing.T) {
			docsCount := types.DocumentCount(curTest.collBytes / kibibyte)

			docsPerPartition := docsPerPartitionFor(
				curTest.collBytes,
				docsCount,
				partitionSize,
			)

			partitionCount := int64(docsCount) / docsPerPartition

			assert.GreaterOrEqual(t, partitionCount, curTest.expectedPartitionsMin)
			assert.LessOrEqual(t, partitionCount, curTest.expectedPartitionsMax)
		})
	}
}
