package oidcfake

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// Browser drives a sign-in the way a browser does: it keeps cookies and doesn't follow redirects on its own.
type Browser struct {
	Client *http.Client
}

// NewBrowser returns a browser with an empty cookie jar.
func NewBrowser(t testing.TB) *Browser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	return &Browser{Client: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}}
}

// Redirect GETs u and returns where it redirects to (it must be a 302).
func (b *Browser) Redirect(t testing.TB, u string) string {
	t.Helper()
	resp, err := b.Client.Get(u)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusFound, resp.StatusCode, "GET %s", u)
	loc := resp.Header.Get("Location")
	require.NotEmpty(t, loc)
	return loc
}

// Start GETs the server's sign-in start URL and returns the provider's authorization URL.
func (b *Browser) Start(t testing.TB, startURL string) string { return b.Redirect(t, startURL) }

// Authorize visits the provider's authorization URL and returns the server's callback URL.
func (b *Browser) Authorize(t testing.TB, authURL string) string { return b.Redirect(t, authURL) }

// Callback visits the callback URL and returns the fragment of the redirect to the web UI.
func (b *Browser) Callback(t testing.TB, callbackURL string) (uiURL string, fragment url.Values) {
	t.Helper()
	loc := b.Redirect(t, callbackURL)
	u, err := url.Parse(loc)
	require.NoError(t, err)
	f, err := url.ParseQuery(u.Fragment)
	require.NoError(t, err)
	u.Fragment = ""
	return u.String(), f
}

// SignIn runs a whole sign-in from startURL and returns the fragment the web UI gets.
func (b *Browser) SignIn(t testing.TB, startURL string) url.Values {
	t.Helper()
	_, f := b.Callback(t, b.Authorize(t, b.Start(t, startURL)))
	return f
}
