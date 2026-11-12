package testutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

type fixtureTemplate struct {
	once sync.Once
	data []byte
	err  error
}

var secretTemplate, inboundCredentialTemplate fixtureTemplate

// SecretCommand copies a fixture built once per process into the test's private directory.
func SecretCommand(tb testing.TB, mode string) []string {
	tb.Helper()
	return []string{fixtureCommand(tb, &secretTemplate, "secret-store", "go.kenn.io/msgvault/internal/testutil/secretcommand"), mode}
}

// InboundCredentialCommand copies an inbound credential provider fixture, built
// once per process, into the test's private directory. The helper issues
// tokens[0] on authorize and the next token on each renew.
func InboundCredentialCommand(tb testing.TB, tokens ...string) []string {
	tb.Helper()
	exe := fixtureCommand(tb, &inboundCredentialTemplate, "inbound-credential", "go.kenn.io/msgvault/internal/testutil/inboundcredential")
	return append([]string{exe}, tokens...)
}

// fixtureCommand builds pkg's cmd subpackage once per process and copies the
// executable into the test's private directory.
func fixtureCommand(tb testing.TB, template *fixtureTemplate, name, pkg string) string {
	tb.Helper()
	template.once.Do(func() {
		dir := tb.TempDir()
		exe := filepath.Join(dir, name)
		if runtime.GOOS == "windows" {
			exe += ".exe"
		}
		fixtureDir, err := exec.CommandContext(tb.Context(), "go", "list", "-f", "{{.Dir}}", pkg).Output() //nolint:gosec // pkg is one of this file's fixed fixture packages.
		if err != nil {
			template.err = fmt.Errorf("locate %s fixture: %w", name, err)
			return
		}
		for _, sub := range []string{".", "cmd"} {
			// This read makes Go's test cache rerun the test when the child's inputs change.
			if _, err := os.ReadDir(filepath.Join(strings.TrimSpace(string(fixtureDir)), sub)); err != nil {
				template.err = fmt.Errorf("list %s fixture: %w", name, err)
				return
			}
		}
		cmd := exec.CommandContext(tb.Context(), "go", "build", "-p=1", "-o", exe, pkg+"/cmd") //nolint:gosec // Build the fixed fixture package in a private scratch directory.
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		output, err := cmd.CombinedOutput()
		if err != nil {
			template.err = fmt.Errorf("build %s fixture: %w: %s", name, err, output)
			return
		}
		template.data, template.err = os.ReadFile(exe)
	})
	require.NoError(tb, template.err)
	exe := filepath.Join(tb.TempDir(), name)
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	require.NoError(tb, os.WriteFile(exe, template.data, 0700)) //nolint:gosec // exe is inside the test's private directory.
	return exe
}

type SecretStoreCommands struct {
	ReadCommand   []string
	WriteCommand  []string
	DeleteCommand []string
	ListCommand   []string
}

func SecretStoreFixture(tb testing.TB) SecretStoreCommands {
	tb.Helper()
	tb.Setenv("MSGVAULT_TEST_SECRET_ROOT", tb.TempDir())
	exe := SecretCommand(tb, "read")[0]
	return SecretStoreCommands{ReadCommand: []string{exe, "read"}, WriteCommand: []string{exe, "write"}, DeleteCommand: []string{exe, "delete"}, ListCommand: []string{exe, "list"}}
}
