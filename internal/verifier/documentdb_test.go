package verifier

import (
	"testing"

	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChangeStreamScope_Covers checks the enablement rules that Amazon
// DocumentDB documents: a collection has change streams enabled if the
// collection itself is enabled, its database is enabled, or all databases are
// enabled. An empty string is a wildcard.
func TestChangeStreamScope_Covers(t *testing.T) {
	for _, curTest := range []struct {
		name     string
		scope    changeStreamScope
		db       string
		coll     string
		expected bool
	}{
		{
			name:     "cluster-wide wildcard covers anything",
			scope:    changeStreamScope{Database: "", Collection: ""},
			db:       "mydb",
			coll:     "mycoll",
			expected: true,
		},
		{
			name:     "database wildcard covers that database",
			scope:    changeStreamScope{Database: "mydb", Collection: ""},
			db:       "mydb",
			coll:     "mycoll",
			expected: true,
		},
		{
			name:     "database wildcard does not cover another database",
			scope:    changeStreamScope{Database: "mydb", Collection: ""},
			db:       "otherdb",
			coll:     "mycoll",
			expected: false,
		},
		{
			name:     "exact namespace is covered",
			scope:    changeStreamScope{Database: "mydb", Collection: "mycoll"},
			db:       "mydb",
			coll:     "mycoll",
			expected: true,
		},
		{
			name:     "sibling collection is not covered",
			scope:    changeStreamScope{Database: "mydb", Collection: "mycoll"},
			db:       "mydb",
			coll:     "othercoll",
			expected: false,
		},
	} {
		t.Run(curTest.name, func(t *testing.T) {
			assert.Equal(
				t,
				curTest.expected,
				curTest.scope.covers(curTest.db, curTest.coll),
			)
		})
	}
}

func TestFindUncoveredNamespaces(t *testing.T) {
	namespaces := mslices.Of("db1.coll1", "db1.coll2", "db2.coll1")

	t.Run("cluster-wide wildcard leaves nothing uncovered", func(t *testing.T) {
		uncovered := findUncoveredNamespaces(
			mslices.Of(changeStreamScope{Database: "", Collection: ""}),
			namespaces,
		)

		assert.Empty(t, uncovered)
	})

	t.Run("reports namespaces no scope enables", func(t *testing.T) {
		uncovered := findUncoveredNamespaces(
			mslices.Of(changeStreamScope{Database: "db1", Collection: ""}),
			namespaces,
		)

		assert.Equal(t, mslices.Of("db2.coll1"), uncovered)
	})

	t.Run("no scopes leaves everything uncovered", func(t *testing.T) {
		uncovered := findUncoveredNamespaces(nil, namespaces)

		assert.Equal(t, namespaces, uncovered)
	})

	t.Run("per-collection scopes combine", func(t *testing.T) {
		uncovered := findUncoveredNamespaces(
			mslices.Of(
				changeStreamScope{Database: "db1", Collection: "coll1"},
				changeStreamScope{Database: "db2", Collection: "coll1"},
			),
			namespaces,
		)

		assert.Equal(t, mslices.Of("db1.coll2"), uncovered)
	})
}

// TestAnyScopeIsClusterWide ensures that enabling each database individually
// does not count as cluster-wide coverage. A verify-all run may encounter a
// database created after it started, which only the wildcard would watch.
func TestAnyScopeIsClusterWide(t *testing.T) {
	assert.True(
		t,
		anyScopeIsClusterWide(
			mslices.Of(
				changeStreamScope{Database: "db1", Collection: ""},
				changeStreamScope{Database: "", Collection: ""},
			),
		),
	)

	assert.False(
		t,
		anyScopeIsClusterWide(
			mslices.Of(
				changeStreamScope{Database: "db1", Collection: ""},
				changeStreamScope{Database: "db2", Collection: ""},
			),
		),
	)

	assert.False(t, anyScopeIsClusterWide(nil))
}

// TestCheckDocumentDBReadPreference ensures the verifier only accepts read
// preferences that route to the primary. Without gossiped cluster time the
// verifier cannot pin reads with afterClusterTime, so it depends on
// DocumentDB’s read-after-write guarantee, which only the primary provides.
func TestCheckDocumentDBReadPreference(t *testing.T) {
	for _, mode := range mslices.Of("primary", "primaryPreferred") {
		t.Run(mode+" is allowed", func(t *testing.T) {
			assert.NoError(t, checkDocumentDBReadPreference(mode))
		})
	}

	for _, mode := range mslices.Of("secondary", "secondaryPreferred", "nearest") {
		t.Run(mode+" is rejected", func(t *testing.T) {
			err := checkDocumentDBReadPreference(mode)

			require.Error(t, err)
			assert.ErrorContains(t, err, "primary")
		})
	}
}

func TestFlavor_IsDocumentDB(t *testing.T) {
	assert.True(t, util.FlavorDocumentDB.IsDocumentDB())
	assert.False(t, util.FlavorMongoDB.IsDocumentDB())
	assert.False(t, util.FlavorAuto.IsDocumentDB())
}

// TestValidateChangeReaderOpt_DocumentDB ensures the oplog reader is rejected
// for a DocumentDB cluster, which has no operations log to tail.
func TestValidateChangeReaderOpt_DocumentDB(t *testing.T) {
	docDBInfo := util.ClusterInfo{
		VersionArray: mslices.Of(5, 0, 0),
		Topology:     util.TopologyReplset,
		Flavor:       util.FlavorDocumentDB,
	}

	err := validateChangeReaderOpt(ChangeReaderOptOplog, docDBInfo)
	require.Error(t, err)
	assert.ErrorContains(t, err, ChangeReaderOptChangeStream)

	assert.NoError(t, validateChangeReaderOpt(ChangeReaderOptChangeStream, docDBInfo))

	mongoInfo := docDBInfo
	mongoInfo.Flavor = util.FlavorMongoDB

	assert.NoError(t, validateChangeReaderOpt(ChangeReaderOptOplog, mongoInfo))
}

// TestSetSrcFlavor ensures the override flag only accepts known flavors.
func TestSetSrcFlavor(t *testing.T) {
	for _, flavor := range append(mslices.Of(util.FlavorAuto), util.Flavors...) {
		t.Run(string(flavor), func(t *testing.T) {
			v := &Verifier{}

			require.NoError(t, v.SetSrcFlavor(flavor))
			assert.Equal(t, flavor, v.srcFlavor)
		})
	}

	t.Run("rejects an unknown flavor", func(t *testing.T) {
		v := &Verifier{}

		err := v.SetSrcFlavor(util.Flavor("cosmosdb"))
		require.Error(t, err)
		assert.ErrorContains(t, err, "cosmosdb")
	})
}
