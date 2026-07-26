package oauth

import (
	"github.com/ory/fosite"
)

// Client is our concrete fosite.Client implementation. It carries the
// metadata persisted in oauth_clients plus an e2a-specific
// CreatedByUserID field used by the DCR-attribution flow.
//
// fosite.Client is an interface; the per-method getters below are what
// fosite reads when validating an authorization request, a token
// exchange, or a revocation. We deliberately don't expose any setters
// outside this package — once a client is loaded from the DB, fosite
// treats it as immutable for the duration of the request.
type Client struct {
	ID                       string
	Name                     string
	RedirectURIs             []string
	GrantTypeStrings         []string
	ResponseTypeStrings      []string
	ScopeStrings             []string
	AudienceStrings          []string
	Public                   bool
	SecretHash               []byte
	TokenEndpointAuthMethodS string
	CreatedByUserID          string
}

// fosite.Client interface methods. We also implement ResponseModeClient
// because OAuth discovery advertises explicit query-mode support. Richer
// subtypes such as OpenIDConnectClient remain deliberately unsupported.

func (c *Client) GetID() string                      { return c.ID }
func (c *Client) GetHashedSecret() []byte            { return c.SecretHash }
func (c *Client) GetRedirectURIs() []string          { return c.RedirectURIs }
func (c *Client) GetGrantTypes() fosite.Arguments    { return fosite.Arguments(c.GrantTypeStrings) }
func (c *Client) GetResponseTypes() fosite.Arguments { return fosite.Arguments(c.ResponseTypeStrings) }
func (c *Client) GetScopes() fosite.Arguments        { return fosite.Arguments(c.ScopeStrings) }
func (c *Client) IsPublic() bool                     { return c.Public }
func (c *Client) GetAudience() fosite.Arguments      { return fosite.Arguments(c.AudienceStrings) }

// GetResponseModes returns the authorization response modes accepted from
// every registered client. The server supports authorization code flow only,
// whose default and sole explicit response mode is query.
func (c *Client) GetResponseModes() []fosite.ResponseModeType {
	return []fosite.ResponseModeType{fosite.ResponseModeDefault, fosite.ResponseModeQuery}
}

// GetTokenEndpointAuthMethod returns the registered auth method.
// fosite reads this opportunistically via a runtime type-assert on
// the AuthMethodClient interface; we keep it because the value is
// meaningful (we only accept "none" for public clients in DCR).
func (c *Client) GetTokenEndpointAuthMethod() string { return c.TokenEndpointAuthMethodS }

// We intentionally do NOT implement OpenIDConnectClient (its
// GetJSONWebKeys signature requires *jose.JSONWebKeySet, not
// interface{}). If OIDC is ever added, those methods land in a
// separate file with the correct types and a separate compile-time
// assertion. Until then, the minimal fosite.Client surface is enough
// for auth_code + PKCE + refresh + revoke.
var _ fosite.Client = (*Client)(nil)
var _ fosite.ResponseModeClient = (*Client)(nil)
