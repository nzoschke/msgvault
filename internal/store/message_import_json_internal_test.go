package store

import (
	jsonv1 "encoding/json"
	"net/mail"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/batchmessageimport"
)

func TestMessageImportSnapshotPreservesLegacyJSON(t *testing.T) {
	message := batchmessageimport.ImportMessage{
		SourceMessageID: "document:1", SourceConversationID: "thread:1",
		Subject: "<Planning> & decisions", BodyText: "Line\u2028paragraph\u2029end",
		SentAt:   time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		Metadata: map[string]any{"z": []string(nil), "a": map[string]any{"empty": map[string]any(nil), "score": 0.5}},
	}
	projection := &messageProjection{
		OriginalMessageID: 1, Headers: mail.Header{"Subject": {message.Subject}},
		Message: map[string]any{"subject": message.Subject, "is_read": true},
	}
	for name, value := range map[string]any{
		"document": message,
		"projection": struct {
			Message    batchmessageimport.ImportMessage `json:"Message"`
			Projection *messageProjection               `json:"Projection"`
		}{message, projection},
	} {
		t.Run(name, func(t *testing.T) {
			// The existing persisted fingerprint was computed from JSON v1 bytes.
			want, err := jsonv1.Marshal(value)
			require.NoError(t, err)
			for range 10 {
				got, err := marshalImportSnapshot(value)
				require.NoError(t, err)
				assert.Equal(t, string(want), string(got))
			}
		})
	}
}
