// Package pairing implements the rport client pairing service inside rportd.
//
// An authenticated API user deposits client credentials and receives a short-lived,
// random pairing code plus one-liners for Linux and Windows. A device then fetches
// <public url>/pairing/<code> (unauthenticated) and gets an installer script with the
// credentials embedded. <public url>/pairing/update serves the client update script.
//
// The scripts and their rendering are taken from the standalone rport-pairing service
// (MIT licensed, see templates/header.txt).
package pairing

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Deposit holds the client credentials embedded in an installer.
type Deposit struct {
	ConnectURL  string `json:"connect_url"`
	Fingerprint string `json:"fingerprint"`
	ClientID    string `json:"client_id"`
	Password    string `json:"password"`
}

// Response is returned for a successful deposit. Its format equals the one of the
// standalone pairing service, so the UI handles both the same way.
type Response struct {
	PairingCode string    `json:"pairing_code"`
	Expires     time.Time `json:"expires"`
	Installers  struct {
		Linux   string `json:"linux"`
		Windows string `json:"windows"`
	} `json:"installers"`
}

// maxFieldLen limits each deposited value; real values are far shorter.
const maxFieldLen = 1024

var fingerprintRe = regexp.MustCompile(`^[0-9a-fA-F]{2}(:[0-9a-fA-F]{2}){15}$`)

// Validate checks the deposit the same way the standalone service did (required fields,
// URL, 47 char fingerprint) and additionally rejects control characters and overlong values.
func (d Deposit) Validate() error {
	var msgs []string
	fields := []struct{ name, value string }{
		{"connect_url", d.ConnectURL},
		{"fingerprint", d.Fingerprint},
		{"client_id", d.ClientID},
		{"password", d.Password},
	}
	for _, f := range fields {
		switch {
		case f.value == "":
			msgs = append(msgs, fmt.Sprintf("'%s' is required", f.name))
		case len(f.value) > maxFieldLen:
			msgs = append(msgs, fmt.Sprintf("'%s' is too long", f.name))
		case strings.IndexFunc(f.value, unicode.IsControl) >= 0:
			msgs = append(msgs, fmt.Sprintf("'%s' must not contain control characters", f.name))
		}
	}
	if d.ConnectURL != "" {
		if err := ValidateHTTPURL(d.ConnectURL); err != nil {
			msgs = append(msgs, fmt.Sprintf("'connect_url' %v", err))
		}
	}
	if d.Fingerprint != "" && !fingerprintRe.MatchString(d.Fingerprint) {
		msgs = append(msgs, "'fingerprint' must be 16 colon separated hex bytes (47 chars)")
	}
	if len(msgs) > 0 {
		return errors.New(strings.Join(msgs, "; "))
	}
	return nil
}

// ValidateHTTPURL checks that s is an absolute http(s) URL with a host.
func ValidateHTTPURL(s string) error {
	u, err := url.ParseRequestURI(s)
	if err != nil {
		return fmt.Errorf("must be a valid URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("must be an http(s) URL, got %q", s)
	}
	if u.Host == "" {
		return fmt.Errorf("must have a host, got %q", s)
	}
	return nil
}

// bashEscaper escapes for a double-quoted POSIX sh/bash string in a single pass, so a
// backslash added for one character is never escaped again. Backticks must be escaped
// too: inside double quotes they still run command substitution.
var bashEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`")

// psEscaper escapes for a double-quoted PowerShell string in a single pass. PowerShell
// also treats the typographic double quotes as string delimiters.
var psEscaper = strings.NewReplacer(
	"`", "``", `"`, "`\"", "$", "`$",
	"“", "`“", "”", "`”", "„", "`„",
)

// EscapeForBash escapes a value for use inside a double-quoted sh/bash string.
func EscapeForBash(in string) string { return bashEscaper.Replace(in) }

// EscapeForPowerShell escapes a value for use inside a double-quoted PowerShell string.
func EscapeForPowerShell(in string) string { return psEscaper.Replace(in) }

func (d Deposit) escaped(esc func(string) string) Deposit {
	return Deposit{
		ConnectURL:  esc(d.ConnectURL),
		Fingerprint: esc(d.Fingerprint),
		ClientID:    esc(d.ClientID),
		Password:    esc(d.Password),
	}
}
