package cmd

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSyncJSONProgressKeepsLineDelimitedRecords(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	var output bytes.Buffer
	progress := syncJSONProgress{output: &output, logger: slog.New(slog.DiscardHandler)}
	progress.OnStart(2)
	progress.OnProgress(1, 1, 0)
	requirements.True(strings.HasSuffix(output.String(), "\n"))
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	requirements.Len(lines, 2)
	assertions.JSONEq(`{"event":"started","total":2}`, lines[0])
	assertions.JSONEq(`{"event":"progress","processed":1,"added":1}`, lines[1])
}

func TestSyncJSONMicrosoftKeepsZeroCounters(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	zero := int64(0)
	var output bytes.Buffer
	err := json.MarshalEncode(jsontext.NewEncoder(&output), syncJSONResult{
		Folders: &zero, MessagesMoved: &zero, MessagesDeleted: &zero,
	})
	requirements.NoError(err)
	var record map[string]jsontext.Value
	requirements.NoError(json.Unmarshal(output.Bytes(), &record))
	assertions.JSONEq("0", string(record["folders"]))
	assertions.JSONEq("0", string(record["messages_moved"]))
	assertions.JSONEq("0", string(record["messages_deleted"]))
}
