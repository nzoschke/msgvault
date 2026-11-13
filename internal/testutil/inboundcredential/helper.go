// Package inboundcredential is a test-only inbound credential provider. It
// speaks the real protocol from pkg/credentialprovider so tests exercise the
// production exchange rather than a stubbed token source.
package inboundcredential

import (
	"os"
	"time"
	"uuid"

	"go.kenn.io/msgvault/pkg/credentialprovider"
)

var refusals = map[credentialprovider.Status]string{
	credentialprovider.StatusSignInRequired:   "no_current_sign_in",
	credentialprovider.StatusApprovalRequired: "account_approval_required",
	credentialprovider.StatusConflict:         "generation_changed",
	credentialprovider.StatusUnavailable:      "authority_unavailable",
}

// Run answers one request. Its arguments are the bearer tokens to issue by
// generation: the first for authorize, the next for each renew, repeating the
// last once they run out. A token named after a refusal status, such as
// sign_in_required, refuses instead.
func Run() {
	tokens := os.Args[1:]
	if len(tokens) == 0 {
		os.Exit(2)
	}
	r, err := credentialprovider.DecodeInboundRequest(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	generation := r.ExpectedGeneration + 1
	token := tokens[min(int(generation), len(tokens))-1]
	if reason, ok := refusals[credentialprovider.Status(token)]; ok {
		err = credentialprovider.WriteInboundResponse(os.Stdout, r, credentialprovider.InboundResponse{
			Version: r.Version, Operation: r.Operation, RequestID: r.RequestID, Status: credentialprovider.Status(token), Reason: reason,
		})
		if err != nil {
			os.Exit(1)
		}
		return
	}
	issued := time.Now().UTC()
	err = credentialprovider.WriteInboundResponse(os.Stdout, r, credentialprovider.InboundResponse{
		InboundScope: r.InboundScope, Version: r.Version, Operation: r.Operation, RequestID: r.RequestID,
		Status: credentialprovider.StatusReady, CredentialID: uuid.New(), CredentialGeneration: generation,
		IssuedAt: issued, ExpiresAt: issued.Add(time.Hour),
		Credential: &credentialprovider.Credential{Type: "bearer", Token: token},
	})
	if err != nil {
		os.Exit(1)
	}
}
