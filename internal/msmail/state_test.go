package msmail

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSyncMailStateFollowsFoldersAndCategories(t *testing.T) {
	f := newFakeGraph(t)
	f.categories = map[string][]string{"m1": {"Next"}}
	f.put("m1", "inbox")
	st := testutil.NewTestStore(t)
	_, err := f.sync(t, st)
	require.NoError(t, err)
	state := func() map[string]any {
		var raw string
		require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT metadata FROM messages WHERE source_message_id=?`), "m1").Scan(&raw))
		var document map[string]map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &document))
		return document["_msgvault_msmail"]
	}
	got := state()
	require.Equal(t, "m1", got["id"])
	require.Equal(t, true, got["inInbox"])
	require.Equal(t, "inbox", got["parentFolderId"])
	require.Equal(t, []any{"Next"}, got["categories"])
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE messages SET metadata='null' WHERE source_message_id=?`), "m1")
	require.NoError(t, err)
	f.mu.Lock()
	delete(f.categories, "m1")
	f.mu.Unlock()
	f.put("m1", "archive")
	_, err = f.sync(t, st)
	require.NoError(t, err)
	got = state()
	require.Equal(t, false, got["inInbox"])
	require.Equal(t, "archive", got["parentFolderId"])
	require.Equal(t, []any{"Next"}, got["categories"])
	f.mu.Lock()
	f.categories["m1"] = []string{}
	f.mu.Unlock()
	f.put("m1", "archive")
	_, err = f.sync(t, st)
	require.NoError(t, err)
	require.Empty(t, state()["categories"])
}
