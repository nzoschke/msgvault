package cmd

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
)

// migrateOutlookMaxInput bounds the mapping JSON: about a million messages.
const migrateOutlookMaxInput = 256 << 20

// outlookMigrationRequest is the migrate-outlook input.
type outlookMigrationRequest struct {
	ArchiveUID string                    `json:"archive_uid"`
	SourceID   int64                     `json:"source_id"`
	Account    string                    `json:"account"`
	Messages   []outlookMigrationMessage `json:"messages"`
	Unmapped   []outlookMigrationKept    `json:"unmapped"`
}

type outlookMigrationMessage struct {
	MessageID         int64          `json:"message_id"`
	SourceMessageID   string         `json:"source_message_id"`
	ProviderMessageID string         `json:"provider_message_id"`
	Metadata          jsontext.Value `json:"metadata"`
}

type outlookMigrationKept struct {
	MessageID       int64  `json:"message_id"`
	SourceMessageID string `json:"source_message_id"`
}

type outlookMigrationResult struct {
	ArchiveUID string `json:"archive_uid"`
	SourceID   int64  `json:"source_id"`
	Messages   int    `json:"messages"`
	Changed    int    `json:"changed"`
}

func (in outlookMigrationRequest) migration() store.SourceIdentityMigration {
	out := store.SourceIdentityMigration{
		ArchiveUID: in.ArchiveUID, SourceID: in.SourceID, Identifier: in.Account,
		FromSourceType: sourceTypeOutlookImport, ToSourceType: sourceTypeMSMail, MetadataKey: "_msgvault_outlook_migration",
	}
	for _, m := range in.Messages {
		out.Messages = append(out.Messages, store.SourceMigrationMessage{
			MessageID: m.MessageID, SourceMessageID: m.SourceMessageID, ProviderMessageID: m.ProviderMessageID, Metadata: m.Metadata,
		})
	}
	for _, k := range in.Unmapped {
		out.Kept = append(out.Kept, store.SourceMigrationKept{MessageID: k.MessageID, SourceMessageID: k.SourceMessageID})
	}
	return out
}

// decodeOutlookMigration reads and validates one request.
func decodeOutlookMigration(r io.Reader) (outlookMigrationRequest, []byte, error) {
	var in outlookMigrationRequest
	data, err := io.ReadAll(io.LimitReader(r, migrateOutlookMaxInput+1))
	if err != nil {
		return in, nil, err
	}
	if len(data) > migrateOutlookMaxInput {
		return in, nil, errors.New("migration mapping exceeds 256 MiB")
	}
	if err := json.Unmarshal(data, &in, json.RejectUnknownMembers(true)); err != nil {
		return in, nil, fmt.Errorf("decode migration mapping: %w", err)
	}
	if err := in.migration().Validate(); err != nil {
		return in, nil, err
	}
	return in, data, nil
}

func newMigrateOutlookCmd() *cobra.Command {
	var input string
	command := &cobra.Command{
		Use:   "migrate-outlook",
		Short: "Convert an Outlook EML import into a native Microsoft Graph mail source",
		Long: `Convert a source of Outlook mail imported as EML files (source type
"outlook") into a native Microsoft Graph mail source (source type "msmail"),
from a reviewed JSON mapping on stdin or in --input.

The mapping names the archive, the source, and the account, and lists every
message of the source exactly once: in "messages", with its current
source_message_id, its Graph immutable ID (provider_message_id), and its Graph
state (metadata, an object whose id equals provider_message_id); or in
"unmapped", with its current source_message_id, to keep that ID.

  {"archive_uid": "...", "source_id": 7, "account": "user@example.com",
   "messages": [{"message_id": 1, "source_message_id": "...",
                 "provider_message_id": "AAMk...", "metadata": {"id": "AAMk...", ...}}],
   "unmapped": [{"message_id": 2, "source_message_id": "..."}]}

In one transaction it changes the source type, rekeys each mapped message to
its Graph ID, and stores the metadata under messages.metadata._msgvault_outlook_migration.
This metadata is migration provenance, not live Graph state.
Source and message IDs, raw MIME, and labels stay. It downloads nothing and
writes nothing to the provider. Any mismatch changes nothing: a different
archive, source, or account, a duplicate or ambiguous mapping, an incomplete
mapping, or an existing msmail source for the account. Repeating a completed
migration with the same mapping succeeds without changes.

Prints {"archive_uid", "source_id", "messages", "changed"}. Run msgvault sync
afterwards; the first sync walks every folder and refreshes native folder and category labels.
It may download drafts or changed content again.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !isDaemonCLISubprocess() {
				return runMigrateOutlookClient(cmd, input)
			}
			return runMigrateOutlookLocal(cmd, input)
		},
	}
	command.Flags().StringVar(&input, "input", "", "Absolute path of the mapping JSON (default: stdin)")
	return command
}

// runMigrateOutlookClient validates the mapping, then has the daemon apply it
// from a private file, because the daemon does not read this process's stdin.
func runMigrateOutlookClient(cmd *cobra.Command, input string) error {
	reader := cmd.InOrStdin()
	if input != "" {
		f, err := os.Open(input)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		reader = f
	}
	_, data, err := decodeOutlookMigration(reader)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "msgvault-migrate-outlook-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "mapping.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	if err := cmd.Flags().Set("input", path); err != nil {
		return fmt.Errorf("pass the mapping to the daemon: %w", err)
	}
	return runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd, nil)
}

func runMigrateOutlookLocal(cmd *cobra.Command, input string) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	reader := cmd.InOrStdin()
	if input != "" {
		data, err := os.ReadFile(input)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	in, _, err := decodeOutlookMigration(reader)
	if err != nil {
		return err
	}
	st, cleanup, err := openWritableStoreAndInitForInvocation(cmd.Context(), state)
	if err != nil {
		return err
	}
	defer cleanup()
	out, err := st.MigrateSourceIdentityContext(cmd.Context(), in.migration())
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(outlookMigrationResult{
		ArchiveUID: in.ArchiveUID, SourceID: in.SourceID, Messages: out.Messages, Changed: out.Changed,
	})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\n", encoded); err != nil {
		return fmt.Errorf("write migration result: %w", err)
	}
	return nil
}

func init() { rootCmd.AddCommand(newMigrateOutlookCmd()) }
