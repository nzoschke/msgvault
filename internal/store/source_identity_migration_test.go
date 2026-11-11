package store_test

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type migrationFixture struct {
	archive  *store.Store
	sourceID int64
	ids      []int64 // m1, m2, m3
	in       store.SourceIdentityMigration
}

func newMigrationFixture(t *testing.T) migrationFixture {
	t.Helper()
	archive := testutil.NewTestStore(t)
	source, err := archive.GetOrCreateSource("outlook", "owner@example.com")
	require.NoError(t, err)
	conversation, err := archive.EnsureConversation(t.Context(), source.ID, "thread", "Example")
	require.NoError(t, err)
	var ids []int64
	for _, raw := range []string{"eml-1", "eml-2", "eml-3"} {
		id, err := archive.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: raw, MessageType: "email"})
		require.NoError(t, err)
		ids = append(ids, id)
	}
	category, err := archive.EnsureLabel(t.Context(), source.ID, "msmail-category:Red", "Category: Red", "user")
	require.NoError(t, err)
	require.NoError(t, archive.AddMessageLabels(t.Context(), ids[0], []int64{category}))
	require.NoError(t, archive.SetMessageMetadata(ids[0], nullString(`{"other":1}`)))
	require.NoError(t, archive.UpsertMessageRawWithFormatContext(t.Context(), ids[0], []byte("Subject: Migration fixture\r\n\r\nOriginal MIME\r\n"), "mime"))
	uid, err := archive.ArchiveUID()
	require.NoError(t, err)
	meta := func(id string) jsontext.Value {
		return jsontext.Value(`{"id":"` + id + `","conversationId":"c","isDraft":false,"parentFolderId":"inbox","receivedDateTime":"2024-01-01T10:00:00Z","categories":["Red"],"inInbox":true}`)
	}
	return migrationFixture{archive: archive, sourceID: source.ID, ids: ids, in: store.SourceIdentityMigration{
		ArchiveUID: uid, SourceID: source.ID, Identifier: "owner@example.com",
		FromSourceType: "outlook", ToSourceType: "msmail", MetadataKey: "_msgvault_outlook_migration",
		Messages: []store.SourceMigrationMessage{
			{MessageID: ids[0], SourceMessageID: "eml-1", ProviderMessageID: "AAMk-1", Metadata: meta("AAMk-1")},
			{MessageID: ids[1], SourceMessageID: "eml-2", ProviderMessageID: "AAMk-2", Metadata: meta("AAMk-2")},
		},
		Kept: []store.SourceMigrationKept{{MessageID: ids[2], SourceMessageID: "eml-3"}},
	}}
}

func (f migrationFixture) snapshot(t *testing.T) (string, map[int64]string) {
	t.Helper()
	var sourceType string
	require.NoError(t, f.archive.DB().QueryRowContext(t.Context(), f.archive.Rebind(`SELECT source_type FROM sources WHERE id = ?`), f.sourceID).Scan(&sourceType))
	rows, err := f.archive.DB().QueryContext(t.Context(), f.archive.Rebind(`SELECT id, source_message_id FROM messages WHERE source_id = ?`), f.sourceID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	keys := map[int64]string{}
	for rows.Next() {
		var id int64
		var key string
		require.NoError(t, rows.Scan(&id, &key))
		keys[id] = key
	}
	require.NoError(t, rows.Err())
	return sourceType, keys
}

func TestMigrateSourceIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newMigrationFixture(t)
	out, err := f.archive.MigrateSourceIdentityContext(t.Context(), f.in)
	require.NoError(err)
	assert.Equal(store.SourceIdentityMigrationResult{Messages: 3, Changed: 2}, out)

	sourceType, keys := f.snapshot(t)
	assert.Equal("msmail", sourceType)
	assert.Equal(map[int64]string{f.ids[0]: "AAMk-1", f.ids[1]: "AAMk-2", f.ids[2]: "eml-3"}, keys)
	assert.Equal([]string{"Category: Red"}, adoptionLabelNames(t, f.archive, f.ids[0]), "labels stay")
	raw, err := f.archive.GetMessageRawContext(t.Context(), f.ids[0])
	require.NoError(err)
	assert.Equal("Subject: Migration fixture\r\n\r\nOriginal MIME\r\n", string(raw))
	meta, err := f.archive.GetMessageMetadata(f.ids[0])
	require.NoError(err)
	assert.JSONEq(`{"other":1,"_msgvault_outlook_migration":{"id":"AAMk-1","conversationId":"c","isDraft":false,"parentFolderId":"inbox","receivedDateTime":"2024-01-01T10:00:00Z","categories":["Red"],"inInbox":true}}`, meta.String)
	source, err := f.archive.GetSourceByTypeAndIdentifier(t.Context(), "msmail", "owner@example.com")
	require.NoError(err)
	assert.Equal(f.sourceID, source.ID, "the source keeps its ID")

	// A repeat is a no-op even when the recorded provenance has changed.
	require.NoError(f.archive.SetMessageMetadata(f.ids[1], nullString(`{"_msgvault_outlook_migration":{"id":"AAMk-2","categories":[]}}`)))
	again, err := f.archive.MigrateSourceIdentityContext(t.Context(), f.in)
	require.NoError(err)
	assert.Equal(store.SourceIdentityMigrationResult{Messages: 3, Changed: 0}, again)

	// A different mapping for the migrated source is a conflict.
	f.in.Messages[1].ProviderMessageID, f.in.Messages[1].Metadata = "AAMk-9", jsontext.Value(`{"id":"AAMk-9"}`)
	_, err = f.archive.MigrateSourceIdentityContext(t.Context(), f.in)
	require.ErrorIs(err, store.ErrSourceMigrationConflict)
}

func TestMigrateSourceIdentityRejectsAndChangesNothing(t *testing.T) {
	for _, tt := range []struct {
		name  string
		edit  func(*testing.T, *migrationFixture)
		error error
	}{
		{"archive", func(t *testing.T, f *migrationFixture) {
			t.Helper()
			f.in.ArchiveUID = "other"
		}, store.ErrSourceMigrationConflict},
		{"account", func(t *testing.T, f *migrationFixture) {
			t.Helper()
			f.in.Identifier = "other@example.com"
		}, store.ErrSourceMigrationConflict},
		{"source", func(t *testing.T, f *migrationFixture) {
			t.Helper()
			f.in.SourceID += 100
		}, store.ErrSourceMigrationConflict},
		{"incomplete", func(t *testing.T, f *migrationFixture) {
			t.Helper()
			f.in.Kept = nil
		}, store.ErrSourceMigrationConflict},
		{"stale last message", func(t *testing.T, f *migrationFixture) {
			t.Helper()
			f.in.Messages[1].SourceMessageID = "eml-old"
		}, store.ErrSourceMigrationConflict},
		{"message of another source", func(t *testing.T, f *migrationFixture) {
			t.Helper()

			other, err := f.archive.GetOrCreateSource("outlook", "other@example.com")
			require.NoError(t, err)
			conversation, err := f.archive.EnsureConversation(t.Context(), other.ID, "t2", "x")
			require.NoError(t, err)
			id, err := f.archive.UpsertMessage(&store.Message{SourceID: other.ID, ConversationID: conversation, SourceMessageID: "eml-x", MessageType: "email"})
			require.NoError(t, err)
			f.in.Kept = []store.SourceMigrationKept{{MessageID: id, SourceMessageID: "eml-x"}}
		}, store.ErrSourceMigrationConflict},
		{"existing native source", func(t *testing.T, f *migrationFixture) {
			t.Helper()

			_, err := f.archive.GetOrCreateSource("msmail", "owner@example.com")
			require.NoError(t, err)
		}, store.ErrSourceMigrationConflict},
		{"wrong source type", func(t *testing.T, f *migrationFixture) {
			t.Helper()
			f.in.FromSourceType = "imap"
		}, store.ErrSourceMigrationConflict},
		{"one raw ID, two Graph IDs", func(t *testing.T, f *migrationFixture) {
			t.Helper()

			f.in.Messages[1].SourceMessageID = "eml-1"
		}, store.ErrSourceMigrationValidation},
		{"duplicate Graph ID", func(t *testing.T, f *migrationFixture) {
			t.Helper()

			f.in.Messages[1].ProviderMessageID, f.in.Messages[1].Metadata = "AAMk-1", jsontext.Value(`{"id":"AAMk-1"}`)
		}, store.ErrSourceMigrationValidation},
		{"duplicate message", func(t *testing.T, f *migrationFixture) {
			t.Helper()
			f.in.Kept[0].MessageID = f.ids[0]
		}, store.ErrSourceMigrationValidation},
		{"metadata ID", func(t *testing.T, f *migrationFixture) {
			t.Helper()
			f.in.Messages[0].Metadata = jsontext.Value(`{"id":"AAMk-2"}`)
		}, store.ErrSourceMigrationValidation},
		{"metadata not an object", func(t *testing.T, f *migrationFixture) {
			t.Helper()
			f.in.Messages[0].Metadata = jsontext.Value(`[]`)
		}, store.ErrSourceMigrationValidation},
		{"Graph ID reuses a raw ID", func(t *testing.T, f *migrationFixture) {
			t.Helper()

			f.in.Messages[0].ProviderMessageID, f.in.Messages[0].Metadata = "eml-3", jsontext.Value(`{"id":"eml-3"}`)
		}, store.ErrSourceMigrationValidation},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newMigrationFixture(t)
			beforeType, beforeKeys := f.snapshot(t)
			tt.edit(t, &f)
			_, err := f.archive.MigrateSourceIdentityContext(t.Context(), f.in)
			require.ErrorIs(err, tt.error)
			afterType, afterKeys := f.snapshot(t)
			assert.Equal(beforeType, afterType)
			assert.Equal(beforeKeys, afterKeys)
			meta, err := f.archive.GetMessageMetadata(f.ids[0])
			require.NoError(err)
			assert.JSONEq(`{"other":1}`, meta.String)
		})
	}
}

func TestMigrateSourceIdentityRollsBackAfterWrite(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newMigrationFixture(t)
	requirements. // A valid JSON scalar cannot be merged as an object. This failure happens
		// after the source and first mapped message have already been updated.
		NoError(f.archive.SetMessageMetadata(f.ids[1], nullString(`42`)))
	_, err := f.archive.MigrateSourceIdentityContext(t.Context(), f.in)
	requirements.Error(err)
	sourceType, keys := f.snapshot(t)
	assertions.Equal("outlook", sourceType)
	assertions.Equal("eml-1", keys[f.ids[0]])
	meta, err := f.archive.GetMessageMetadata(f.ids[0])
	requirements.NoError(err)
	assertions.JSONEq(`{"other":1}`, meta.String)
}

func TestMigrateSourceIdentityPreservesMatchingMetadata(t *testing.T) {
	requirements := require.New(t)

	f := newMigrationFixture(t)
	metadata := `{"other":1,"_msgvault_outlook_migration":` + string(f.in.Messages[0].Metadata) + `}`
	requirements.NoError(f.archive.SetMessageMetadata(f.ids[0], nullString(metadata)))
	_, err := f.archive.MigrateSourceIdentityContext(t.Context(), f.in)
	requirements.NoError(err)
	got, err := f.archive.GetMessageMetadata(f.ids[0])
	requirements.NoError(err)
	assert.JSONEq(t, metadata, got.String)
}
