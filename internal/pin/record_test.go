package pin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecord_Pinned(t *testing.T) {
	rec := &Record{
		Entries: []Entry{
			{NWO: "actions/checkout", Ref: "v4", Resolution: Pinned},
			{NWO: "actions/setup-go", Ref: "v5", Resolution: Verified},
			{NWO: "actions/cache", Ref: "v3", Resolution: Pinned},
			{NWO: "owner/bad", Ref: "v1", Resolution: Investigate},
		},
	}
	got := rec.Pinned()
	require.Len(t, got, 2)
	assert.Equal(t, "actions/checkout", got[0].NWO)
	assert.Equal(t, "actions/cache", got[1].NWO)
}

func TestRecord_Investigated(t *testing.T) {
	rec := &Record{
		Entries: []Entry{
			{NWO: "a/b", Ref: "v1", Resolution: Pinned},
			{NWO: "c/d", Ref: "v2", Resolution: Investigate},
		},
	}
	got := rec.Investigated()
	require.Len(t, got, 1)
	assert.Equal(t, "c/d", got[0].NWO)
}

func TestRecord_Pinned_empty(t *testing.T) {
	rec := &Record{}
	assert.Empty(t, rec.Pinned())
}

func TestRecord_Valid(t *testing.T) {
	tests := []struct {
		name    string
		entries []Entry
		want    bool
	}{
		{"empty record is valid", nil, true},
		{"all pinned is valid", []Entry{
			{Resolution: Pinned}, {Resolution: Verified},
		}, true},
		{"investigate makes invalid", []Entry{
			{Resolution: Pinned}, {Resolution: Investigate},
		}, false},
		{"unresolved makes invalid", []Entry{
			{Resolution: Unresolved},
		}, false},
		{"skipped is valid", []Entry{
			{Resolution: Skipped},
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &Record{Entries: tt.entries}
			assert.Equal(t, tt.want, rec.Valid())
		})
	}
}
