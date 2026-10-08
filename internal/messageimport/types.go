package messageimport

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const MaxRequestBytes = 16 << 20
const MetadataKey = "_msgvault_import"
const Owner = "messages-v1"
const ProjectionKey = "_msgvault_projection"

var ErrValidation = errors.New("invalid message import")
var ErrConflict = errors.New("message import conflicts with existing archive data")
var sourceTypePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}-v[1-9][0-9]*$`)

type ImportSource struct {
	Type        string `json:"type" minLength:"1" maxLength:"80"`
	Identifier  string `json:"identifier" minLength:"1" maxLength:"512"`
	DisplayName string `json:"display_name,omitempty" maxLength:"512"`
}

type ImportMessage struct {
	OriginalMessageID    int64          `json:"original_message_id,omitempty" minimum:"1"`
	SourceMessageID      string         `json:"source_message_id" minLength:"1" maxLength:"512"`
	SourceConversationID string         `json:"source_conversation_id" minLength:"1" maxLength:"512"`
	Subject              string         `json:"subject" minLength:"1" maxLength:"4096"`
	SentAt               time.Time      `json:"sent_at"`
	BodyText             string         `json:"body_text" minLength:"1" maxLength:"2097152"`
	Metadata             map[string]any `json:"metadata,omitempty"`
}

type ImportMessagesRequest struct {
	Source   ImportSource    `json:"source"`
	Messages []ImportMessage `json:"messages" minItems:"1" maxItems:"100"`
}

type ImportedMessage struct {
	SourceMessageID string `json:"source_message_id"`
	MessageID       int64  `json:"message_id"`
	Status          string `json:"status" enum:"created,unchanged,updated"`
}

type ImportMessagesResponse struct {
	SourceID int64             `json:"source_id"`
	Messages []ImportedMessage `json:"messages"`
}

func (in ImportMessagesRequest) Validate() error {
	valid := func(value string, limit int, required bool) bool {
		return utf8.ValidString(value) && !strings.ContainsRune(value, 0) && len(value) <= limit && (!required || strings.TrimSpace(value) != "")
	}
	if !sourceTypePattern.MatchString(in.Source.Type) || len(in.Source.Type) > 80 {
		return fmt.Errorf("%w: source.type must be a custom versioned name such as prepared-v1", ErrValidation)
	}
	if !valid(in.Source.Identifier, 512, true) || !valid(in.Source.DisplayName, 512, false) {
		return fmt.Errorf("%w: invalid source identity", ErrValidation)
	}
	if len(in.Messages) == 0 || len(in.Messages) > 100 {
		return fmt.Errorf("%w: messages must contain 1 to 100 records", ErrValidation)
	}
	seen := map[string]bool{}
	for i, m := range in.Messages {
		if m.OriginalMessageID < 0 {
			return fmt.Errorf("%w: invalid original message ID", ErrValidation)
		}
		if !valid(m.SourceMessageID, 512, true) || !valid(m.SourceConversationID, 512, true) || seen[m.SourceMessageID] {
			return fmt.Errorf("%w: messages[%d] has an invalid or repeated identity", ErrValidation, i)
		}
		seen[m.SourceMessageID] = true
		if !valid(m.Subject, 4096, true) || !valid(m.BodyText, 2<<20, true) || m.SentAt.IsZero() {
			return fmt.Errorf("%w: messages[%d] requires valid text and a timestamp", ErrValidation, i)
		}
		if _, ok := m.Metadata[MetadataKey]; ok {
			return fmt.Errorf("%w: reserved metadata key", ErrValidation)
		}
		if _, ok := m.Metadata[ProjectionKey]; ok {
			return fmt.Errorf("%w: reserved projection metadata key", ErrValidation)
		}
		data, err := json.Marshal(m.Metadata)
		if err != nil || len(data) > 1<<20 {
			return fmt.Errorf("%w: invalid or oversized metadata", ErrValidation)
		}
	}
	data, err := json.Marshal(in)
	if err != nil || len(data) > MaxRequestBytes {
		return fmt.Errorf("%w: request exceeds 16 MiB", ErrValidation)
	}
	return nil
}
