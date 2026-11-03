package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mayura-andrew/event-pass-platform/internal/domain"
)

// GoogleOAuth signs people in with Google (OpenID Connect, authorization code
// flow with PKCE). No Google library: the flow is two HTTP calls.
type GoogleOAuth struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string // APP_BASE_URL + /auth/google/callback

	// Overridable for tests.
	AuthURL  string
	TokenURL string
	HTTP     *http.Client
}

func NewGoogleOAuth(clientID, clientSecret, redirectURL string) *GoogleOAuth {
	return &GoogleOAuth{
		ClientID: clientID, ClientSecret: clientSecret, RedirectURL: redirectURL,
		AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL: "https://oauth2.googleapis.com/token",
		HTTP:     &http.Client{Timeout: 10 * time.Second},
	}
}

// GoogleIdentity is a verified Google account.
type GoogleIdentity struct {
	Subject string
	Email   string
	Name    string
	Picture string
}

// AuthCodeURL is where the browser is sent to sign in. state guards the
// callback against forgery, nonce binds the ID token to this attempt, and
// verifier is the PKCE secret kept in the browser's cookie.
func (g *GoogleOAuth) AuthCodeURL(state, nonce, verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id":             {g.ClientID},
		"redirect_uri":          {g.RedirectURL},
		"response_type":         {"code"},
		"scope":                 {"openid email profile"},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"prompt":                {"select_account"},
	}
	return g.AuthURL + "?" + q.Encode()
}

// Exchange trades the callback's code for the ID token and checks its claims.
//
// The signature is not checked: the token comes straight from Google's token
// endpoint over TLS in exchange for our client secret, which OpenID Connect
// Core 3.1.3.7 allows in place of signature validation.
func (g *GoogleOAuth) Exchange(ctx context.Context, code, verifier, nonce string) (*GoogleIdentity, error) {
	form := url.Values{
		"code":          {code},
		"client_id":     {g.ClientID},
		"client_secret": {g.ClientSecret},
		"redirect_uri":  {g.RedirectURL},
		"grant_type":    {"authorization_code"},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := g.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("google token request: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google token endpoint: HTTP %d: %.200s", res.StatusCode, body)
	}
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.IDToken == "" {
		return nil, errors.New("google token response has no id_token")
	}
	return g.checkIDToken(tok.IDToken, nonce, time.Now())
}

func (g *GoogleOAuth) checkIDToken(idToken, nonce string, now time.Time) (*GoogleIdentity, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed id_token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("malformed id_token payload")
	}
	var c struct {
		Iss           string          `json:"iss"`
		Aud           string          `json:"aud"`
		Exp           int64           `json:"exp"`
		Sub           string          `json:"sub"`
		Email         string          `json:"email"`
		EmailVerified json.RawMessage `json:"email_verified"` // true or "true"
		Name          string          `json:"name"`
		Picture       string          `json:"picture"`
		Nonce         string          `json:"nonce"`
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, errors.New("malformed id_token claims")
	}
	switch {
	case c.Iss != "https://accounts.google.com" && c.Iss != "accounts.google.com":
		return nil, fmt.Errorf("id_token issuer %q", c.Iss)
	case c.Aud != g.ClientID:
		return nil, errors.New("id_token is for another client")
	case now.Unix() > c.Exp:
		return nil, errors.New("id_token expired")
	case c.Nonce == "" || c.Nonce != nonce:
		return nil, errors.New("id_token nonce mismatch")
	case c.Sub == "":
		return nil, errors.New("id_token has no subject")
	}
	if v := strings.Trim(string(c.EmailVerified), `"`); v != "true" || c.Email == "" {
		return nil, fmt.Errorf("%w: your Google account's email address isn't verified", domain.ErrValidation)
	}
	return &GoogleIdentity{
		Subject: c.Sub, Email: strings.ToLower(c.Email), Name: c.Name, Picture: c.Picture,
	}, nil
}

// ---- Integrations: consent for Sheets and Forms (M5) --------------------------

// Google API scopes for integrations. drive.file reaches only spreadsheets
// the platform creates, the narrowest scope that lets it make and update one.
var (
	ScopeSheets = []string{"https://www.googleapis.com/auth/drive.file", "email"}
	ScopeForms  = []string{"https://www.googleapis.com/auth/forms.body.readonly",
		"https://www.googleapis.com/auth/forms.responses.readonly", "email"}
)

// ConsentURL asks for API access with a refresh token (offline), returning
// to redirect. state and the PKCE verifier work as in sign-in.
func (g *GoogleOAuth) ConsentURL(redirect, state, verifier string, scopes []string) string {
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id":              {g.ClientID},
		"redirect_uri":           {redirect},
		"response_type":          {"code"},
		"scope":                  {strings.Join(scopes, " ")},
		"state":                  {state},
		"code_challenge":         {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method":  {"S256"},
		"access_type":            {"offline"},
		"prompt":                 {"consent"}, // always hand out a refresh token
		"include_granted_scopes": {"true"},
	}
	return g.AuthURL + "?" + q.Encode()
}

// Tokens are what the token endpoint returns.
type Tokens struct {
	Access  string
	Refresh string
	Email   string // from the ID token when "email" was granted
}

func (g *GoogleOAuth) tokenRequest(ctx context.Context, form url.Values) (map[string]any, error) {
	form.Set("client_id", g.ClientID)
	form.Set("client_secret", g.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := g.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("google token request: %w", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	if res.StatusCode != http.StatusOK {
		if out["error"] == "invalid_grant" {
			return nil, ErrGoogleRevoked
		}
		return nil, fmt.Errorf("google token endpoint: HTTP %d: %.200s", res.StatusCode, body)
	}
	return out, nil
}

// ErrGoogleRevoked: the refresh token no longer works (access removed in the
// Google account, or expired); the organizer has to connect again.
var ErrGoogleRevoked = errors.New("Google access was removed; connect again")

// ExchangeTokens swaps a consent code for tokens.
func (g *GoogleOAuth) ExchangeTokens(ctx context.Context, code, verifier, redirect string) (*Tokens, error) {
	out, err := g.tokenRequest(ctx, url.Values{"code": {code}, "code_verifier": {verifier}, "redirect_uri": {redirect},
		"grant_type": {"authorization_code"}})
	if err != nil {
		return nil, err
	}
	t := &Tokens{}
	t.Access, _ = out["access_token"].(string)
	t.Refresh, _ = out["refresh_token"].(string)
	if t.Access == "" || t.Refresh == "" {
		return nil, errors.New("google didn't return a refresh token")
	}
	if idt, _ := out["id_token"].(string); idt != "" {
		if parts := strings.Split(idt, "."); len(parts) == 3 {
			if b, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
				var c struct {
					Email string `json:"email"`
				}
				json.Unmarshal(b, &c)
				t.Email = strings.ToLower(c.Email)
			}
		}
	}
	return t, nil
}

// AccessToken gets a fresh access token from a refresh token.
func (g *GoogleOAuth) AccessToken(ctx context.Context, refresh string) (string, error) {
	out, err := g.tokenRequest(ctx, url.Values{"refresh_token": {refresh}, "grant_type": {"refresh_token"}})
	if err != nil {
		return "", err
	}
	tok, _ := out["access_token"].(string)
	if tok == "" {
		return "", errors.New("google returned no access token")
	}
	return tok, nil
}
