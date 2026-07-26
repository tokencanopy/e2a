package agent

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/ory/fosite"
	"golang.org/x/text/language"
)

func TestAuthorizeErrorResponseWriterOmitsEmptyIssuer(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &authorizeErrorResponseWriter{ResponseWriter: rec}
	w.Header().Set("Location", "https://client.example/callback?error=invalid_scope")

	w.WriteHeader(http.StatusSeeOther)

	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Query().Has("iss") {
		t.Errorf("empty issuer must be omitted; Location=%q", location.String())
	}
	if got := location.Query().Get("error"); got != "invalid_scope" {
		t.Errorf("error = %q, want invalid_scope; Location=%q", got, location.String())
	}
	if got := location.String(); got != "https://client.example/callback?error=invalid_scope" {
		t.Errorf("Location = %q, want original redirect preserved", got)
	}
}

func TestQueryModeAuthorizeRequesterPreservesLanguage(t *testing.T) {
	original := fosite.NewAuthorizeRequest()
	original.Lang = language.French

	wrapped := queryModeAuthorizeRequester{AuthorizeRequester: original}

	if got := wrapped.GetLang(); got != language.French {
		t.Errorf("GetLang() = %v, want %v", got, language.French)
	}
}

func TestOAuthIssuerCanonicalizesAPIURL(t *testing.T) {
	for _, tc := range []struct {
		name   string
		apiURL string
		want   string
	}{
		{name: "trailing slashes", apiURL: "https://api.e2a.dev///", want: "https://api.e2a.dev"},
		{name: "empty", apiURL: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &API{apiURL: tc.apiURL}
			if got := api.oauthIssuer(); got != tc.want {
				t.Errorf("oauthIssuer() = %q, want %q", got, tc.want)
			}
		})
	}
}
