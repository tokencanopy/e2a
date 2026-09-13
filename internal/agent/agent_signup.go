package agent

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"math/big"
	"net/url"
	"strings"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

const AgentSignupCodeTTL = 48 * time.Hour

var ErrAgentSignupUnavailable = errors.New("agent signup is unavailable")

// SetAgentSignupSecret enables verification-code hashing. It is deliberately
// the deployment signing secret: no new secret distribution path is needed.
func (a *API) SetAgentSignupSecret(secret string) { a.agentSignupSecret = []byte(secret) }

func (a *API) hashAgentSignupCode(code string) string {
	mac := hmac.New(sha256.New, a.agentSignupSecret)
	_, _ = mac.Write([]byte("agent-signup-code:v1:" + code))
	return hex.EncodeToString(mac.Sum(nil))
}

func generateAgentSignupCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// RegisterAgentSignup provisions or rotates one pending signup and sends the
// human verification mail before returning the one-time plaintext key.
func (a *API) RegisterAgentSignup(ctx context.Context, humanEmail, displayName, noteToHuman, harness string) (*identity.AgentSignupResult, error) {
	if a.store == nil || a.sharedDomain == "" || len(a.agentSignupSecret) == 0 || a.submitter == nil || a.gate == nil || a.smtpRelay == nil || !a.smtpRelay.Configured() || a.fromDomain == "" {
		return nil, ErrAgentSignupUnavailable
	}
	code, err := generateAgentSignupCode()
	if err != nil {
		return nil, err
	}
	user, err := a.store.EnsureAgentSignupUser(ctx, humanEmail, displayName)
	if err != nil {
		return nil, err
	}
	maxAgents := 0
	if a.enforcer != nil {
		resolved, err := a.enforcer.Get(ctx, user.ID)
		if err != nil {
			return nil, err
		}
		maxAgents = resolved.MaxAgents
	}
	result, err := a.store.RegisterAgentSignup(ctx, user.ID, identity.AgentSignupRegistration{
		HumanEmail: humanEmail, DisplayName: displayName, NoteToHuman: noteToHuman,
		Harness: harness, SharedDomain: a.sharedDomain, CodeHash: a.hashAgentSignupCode(code),
		CodeExpiresAt: time.Now().UTC().Add(AgentSignupCodeTTL), MaxAgents: maxAgents,
	})
	if err != nil {
		return nil, err
	}
	if err := a.sendAgentSignupVerification(ctx, result.Signup, code); err != nil {
		// Do not leave a usable credential behind when the public call failed.
		_ = a.store.DeleteAPIKey(context.WithoutCancel(ctx), result.APIKey.ID, user.ID)
		return nil, err
	}
	return result, nil
}

func (a *API) VerifyAgentSignup(ctx context.Context, agentID, code string, reviewOutbound bool) (*identity.AgentSignup, error) {
	if len(a.agentSignupSecret) == 0 {
		return nil, ErrAgentSignupUnavailable
	}
	return a.store.VerifyAgentSignup(ctx, agentID, a.hashAgentSignupCode(code), reviewOutbound, time.Now().UTC())
}

func (a *API) sendAgentSignupVerification(ctx context.Context, signup *identity.AgentSignup, code string) error {
	from := outbound.PlatformEnvelopeFrom(a.fromDomain)
	fromHeader := fmt.Sprintf("%q <%s>", outbound.PlatformDisplayName, from)
	dashboardURL := strings.TrimRight(a.publicURL, "/") + "/agent-signups"
	textBody := "An agent named " + signup.DisplayName + " requested an e2a inbox.\n\nVerification code: " + code
	if signup.NoteToHuman != "" {
		textBody += "\n\nNote from the agent:\n" + signup.NoteToHuman
	}
	if a.publicURL != "" {
		textBody += "\n\nReview this request: " + dashboardURL
	}
	htmlBody := "<p>An agent named <strong>" + html.EscapeString(signup.DisplayName) + "</strong> requested an e2a inbox.</p>" +
		"<p>Verification code: <strong>" + code + "</strong></p>"
	if signup.NoteToHuman != "" {
		htmlBody += "<p>Note from the agent:</p><blockquote>" + strings.ReplaceAll(html.EscapeString(signup.NoteToHuman), "\n", "<br>") + "</blockquote>"
	}
	if a.publicURL != "" {
		htmlBody += `<p><a href="` + html.EscapeString(dashboardURL) + `">Review this request in e2a</a></p>`
	}
	raw, err := outbound.ComposeMultipartMessage(fromHeader, []string{signup.HumanEmail}, nil,
		"Verify your agent's e2a inbox", textBody, htmlBody, "", nil, a.fromDomain, "", "")
	if err != nil {
		return fmt.Errorf("compose signup verification: %w", err)
	}
	// The provider gate binds the anonymous request's only possible egress to
	// the declared human address before the SMTP socket can open.
	submissionID, err := feedbackSubmissionID()
	if err != nil {
		return err
	}
	ref, err := a.gate.PreparePublicFeedback(ctx, sendingpolicy.NewPublicFeedbackRef("agent-signup-"+submissionID, []string{signup.HumanEmail}))
	if err != nil {
		return fmt.Errorf("prepare signup verification: %w", err)
	}
	decision, attempt, err := a.gate.Reserve(ctx, ref)
	if err != nil {
		return fmt.Errorf("reserve signup verification: %w", err)
	}
	if !decision.Allow {
		return fmt.Errorf("reserve signup verification: %s", decision.Reason)
	}
	decision, authz, err := a.gate.ConsumeAttempt(ctx, attempt)
	if err != nil {
		return fmt.Errorf("authorize signup verification: %w", err)
	}
	if !decision.Allow || authz == nil {
		return fmt.Errorf("authorize signup verification: %s", decision.Reason)
	}
	_, err = a.submitter.SubmitOnce(ctx, *authz, outbound.Envelope{From: from, Recipients: authz.AuthorizedRecipients(), Message: raw})
	if err != nil {
		return fmt.Errorf("send signup verification: %w", err)
	}
	return nil
}

// AgentSignupConsoleURL is exposed for clients that want to guide the human
// to the hosted review page without guessing the web origin.
func (a *API) AgentSignupConsoleURL() string {
	if a.publicURL == "" {
		return ""
	}
	u, _ := url.JoinPath(a.publicURL, "agent-signups")
	return u
}
