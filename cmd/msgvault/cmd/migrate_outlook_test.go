package cmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validOutlookMigration = `{"archive_uid":"u","source_id":1,"account":"a@example.com",
	"messages":[{"message_id":1,"source_message_id":"eml-1","provider_message_id":"AAMk","metadata":{"id":"AAMk","categories":[]}}],
	"unmapped":[{"message_id":2,"source_message_id":"eml-2"}]}`

func TestDecodeOutlookMigration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	in, _, err := decodeOutlookMigration(strings.NewReader(validOutlookMigration))
	require.NoError(err)
	migration := in.migration()
	assert.Equal("outlook", migration.FromSourceType)
	assert.Equal("msmail", migration.ToSourceType)
	assert.Equal("_msgvault_outlook_migration", migration.MetadataKey)
	assert.Len(migration.Kept, 1)
}

func TestDecodeOutlookMigrationRejects(t *testing.T) {
	valid := validOutlookMigration
	for name, body := range map[string]string{
		"unknown field":  strings.Replace(valid, `"account"`, `"extra":1,"account"`, 1),
		"metadata id":    strings.Replace(valid, `{"id":"AAMk"`, `{"id":"other"`, 1),
		"duplicate row":  strings.Replace(valid, `"message_id":2`, `"message_id":1`, 1),
		"not an object":  `[]`,
		"missing source": strings.Replace(valid, `"source_id":1`, `"source_id":0`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := decodeOutlookMigration(strings.NewReader(body))
			require.Error(t, err)
		})
	}
}
