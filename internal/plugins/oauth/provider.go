package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Profile is the verified identity a provider returns.
type Profile struct {
	SubjectID string
	Login     string
	Name      string
	Email     string
	Avatar    string
	// Orgs are organisation logins, fetched only when a rule matches on them.
	Orgs []string
}

// Provider implements one IdP's authorization-code flow.
type Provider interface {
	Name() string
	AuthorizeURL(c ClientConfig, state string) string
	Exchange(ctx context.Context, hc *http.Client, c ClientConfig, secret, code string) (Profile, error)
}

var providers = map[string]Provider{}

func registerProvider(p Provider) { providers[p.Name()] = p }

func init() { registerProvider(GitHub{}) }

// GitHub endpoints are variables so tests can point them at a fake server.
var (
	githubAuthorize = "https://github.com/login/oauth/authorize"
	githubToken     = "https://github.com/login/oauth/access_token"
	githubAPI       = "https://api.github.com"
)

// GitHub is the GitHub OAuth provider.
type GitHub struct{}

func (GitHub) Name() string { return "github" }

// AuthorizeURL returns the GitHub consent URL for state.
func (GitHub) AuthorizeURL(c ClientConfig, state string) string {
	q := url.Values{
		"client_id":    {c.ClientID},
		"redirect_uri": {c.RedirectURI},
		"scope":        {githubScope(c)},
		"state":        {state},
		"allow_signup": {"false"},
	}
	return githubAuthorize + "?" + q.Encode()
}

// githubScope asks for organisation membership only when a rule needs it.
func githubScope(c ClientConfig) string {
	if needsOrgs(c.Rules) {
		return "read:user user:email read:org"
	}
	return "read:user user:email"
}

const maxBody = 1 << 20

func getJSON(hc *http.Client, req *http.Request, dst any) error {
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: status %d", req.Method, req.URL.Path, resp.StatusCode)
	}
	return json.Unmarshal(body, dst)
}

// Exchange trades the authorization code for a token and reads the user's profile, verified e-mails
// and organisations.
func (GitHub) Exchange(ctx context.Context, hc *http.Client, c ClientConfig, secret, code string) (Profile, error) {
	form := url.Values{"client_id": {c.ClientID}, "client_secret": {secret}, "code": {code}, "redirect_uri": {c.RedirectURI}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, githubToken, strings.NewReader(form.Encode()))
	if err != nil {
		return Profile{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := getJSON(hc, req, &tok); err != nil {
		return Profile{}, err
	}
	if tok.AccessToken == "" {
		return Profile{}, fmt.Errorf("github token exchange failed: %s", tok.Error)
	}
	api := func(path string, dst any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPI+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", "go-ldap-admin")
		return getJSON(hc, req, dst)
	}
	var user struct {
		ID     int64  `json:"id"`
		Login  string `json:"login"`
		Name   string `json:"name"`
		Avatar string `json:"avatar_url"`
	}
	if err := api("/user", &user); err != nil {
		return Profile{}, err
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := api("/user/emails", &emails); err != nil {
		return Profile{}, err
	}
	p := Profile{SubjectID: strconv.FormatInt(user.ID, 10), Login: user.Login, Name: user.Name, Avatar: user.Avatar}
	for _, e := range emails {
		if e.Primary && e.Verified {
			p.Email = e.Email
		}
	}
	if user.ID <= 0 || p.Email == "" {
		return Profile{}, errors.New("github account has no verified primary email")
	}
	if p.Name == "" {
		p.Name = p.Login
	}
	if needsOrgs(c.Rules) {
		var orgs []struct {
			Login string `json:"login"`
		}
		if err := api("/user/orgs?per_page=100", &orgs); err != nil {
			return Profile{}, err
		}
		for _, o := range orgs {
			p.Orgs = append(p.Orgs, o.Login)
		}
	}
	return p, nil
}

func newHTTPClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
