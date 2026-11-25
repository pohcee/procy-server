package signup

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	zxcvbn "github.com/trustelem/zxcvbn"

	"github.com/openrport/openrport/share/email"
)

const (
	maxEmail   = 254
	maxOrgName = 100
	// maxPassword: bcrypt uses the first 72 bytes only
	maxPassword = 72
	idChars     = "abcdefghijkmnpqrstuvwxyz23456789"
	idSuffixLen = 6
	maxSlug     = 40
)

// normalizeEmail trims and lower-cases an email and returns it with its domain.
func normalizeEmail(s string) (addr, domain string, err error) {
	addr = strings.ToLower(strings.TrimSpace(s))
	if len(addr) > maxEmail {
		return "", "", fmt.Errorf("%w: the email is too long", ErrInvalid)
	}
	if err := email.Validate(addr); err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	at := strings.LastIndexByte(addr, '@')
	domain = addr[at+1:]
	if !strings.Contains(domain, ".") {
		return "", "", fmt.Errorf("%w: the email domain must have a dot", ErrInvalid)
	}
	return addr, domain, nil
}

// PasswordRules are the server's password settings ([api] password_min_length, password_zxcvbn_minscore).
type PasswordRules struct {
	MinLength int
	// MinScore is the least zxcvbn score; negative: no check
	MinScore int
}

func (p PasswordRules) check(password, user string) error {
	if utf8.RuneCountInString(password) < p.MinLength {
		return passwordError{fmt.Sprintf("the password must be at least %d characters", p.MinLength)}
	}
	if len(password) > maxPassword {
		return passwordError{fmt.Sprintf("the password must be at most %d bytes", maxPassword)}
	}
	if p.MinScore >= 0 {
		if zxcvbn.PasswordStrength(password, []string{user}).Score < p.MinScore {
			return passwordError{"the password is too guessable"}
		}
	}
	return nil
}

// Domains decides which email domains may sign up.
type Domains struct {
	blocked map[string]bool
	// allowed, when not nil, is the only domains accepted
	allowed map[string]bool
	// LookupMX, when set, must find a mail server for the domain (net.Resolver).
	LookupMX func(ctx context.Context, domain string) error
}

// LoadDomains reads the blocklist and the allowlist files (either may be ""): one domain per line, "#" comments.
func LoadDomains(blockedFile, allowedFile string) (*Domains, error) {
	d := &Domains{}
	var err error
	if blockedFile != "" {
		if d.blocked, err = readDomains(blockedFile); err != nil {
			return nil, err
		}
	}
	if allowedFile != "" {
		if d.allowed, err = readDomains(allowedFile); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// NewDomains is Domains from lists, for tests and callers with their own source.
func NewDomains(blocked, allowed []string) *Domains {
	d := &Domains{blocked: map[string]bool{}}
	for _, b := range blocked {
		d.blocked[strings.ToLower(b)] = true
	}
	if allowed != nil {
		d.allowed = map[string]bool{}
		for _, a := range allowed {
			d.allowed[strings.ToLower(a)] = true
		}
	}
	return d
}

func readDomains(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("signup: domain list: %w", err)
	}
	defer f.Close()
	return parseDomains(f)
}

func parseDomains(r io.Reader) (map[string]bool, error) {
	out := map[string]bool{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.ToLower(strings.TrimSpace(line))
		if line != "" {
			out[strings.TrimPrefix(line, "@")] = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("signup: domain list: %w", err)
	}
	return out, nil
}

// matches reports whether domain or one of its parent domains is in set.
func matches(set map[string]bool, domain string) bool {
	for d := domain; d != ""; {
		if set[d] {
			return true
		}
		i := strings.IndexByte(d, '.')
		if i < 0 {
			break
		}
		d = d[i+1:]
	}
	return false
}

// Check returns ErrDomain when domain may not sign up.
func (d *Domains) Check(ctx context.Context, domain string) error {
	if d == nil {
		return nil
	}
	if matches(d.blocked, domain) {
		return ErrDomain
	}
	if d.allowed != nil && !matches(d.allowed, domain) {
		return ErrDomain
	}
	if d.LookupMX != nil {
		if err := d.LookupMX(ctx, domain); err != nil {
			return ErrDomain
		}
	}
	return nil
}

// LookupMX finds a mail server for domain: an MX record, or else an address record (the implicit MX).
func LookupMX(ctx context.Context, domain string) error {
	mx, err := net.DefaultResolver.LookupMX(ctx, domain)
	if err == nil && len(mx) > 0 && mx[0].Host != "." {
		return nil
	}
	addrs, err2 := net.DefaultResolver.LookupHost(ctx, domain)
	if err2 == nil && len(addrs) > 0 {
		return nil
	}
	return errors.Join(err, err2, errors.New("no mail server"))
}

// freeMailDomains are large free mail providers: their domain says nothing about the organization, so the signup
// queue skips its domain checks for them.
var freeMailDomains = map[string]bool{}

func init() {
	for _, d := range []string{
		"gmail.com", "googlemail.com", "outlook.com", "hotmail.com", "live.com", "msn.com", "yahoo.com", "ymail.com",
		"icloud.com", "me.com", "mac.com", "aol.com", "proton.me", "protonmail.com", "pm.me", "gmx.com", "gmx.net",
		"gmx.de", "web.de", "mail.com", "yandex.com", "yandex.ru", "zoho.com", "fastmail.com", "tutanota.com",
		"tuta.io", "mail.ru", "qq.com", "163.com", "hotmail.co.uk", "yahoo.co.uk", "outlook.de", "t-online.de",
		"libero.it", "orange.fr", "free.fr", "laposte.net", "hey.com",
	} {
		freeMailDomains[d] = true
	}
}

// IsFreeMail reports whether domain is a large free mail provider.
func IsFreeMail(domain string) bool { return freeMailDomains[strings.ToLower(domain)] }

// mxBudget bounds the MX lookup a signup request records for the queue.
const mxBudget = 3 * time.Second

// newToken returns a random token for a link and the hash that is stored.
func newToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("signup: token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token), nil
}

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	maxIdx := big.NewInt(int64(len(idChars)))
	for i := range b {
		k, err := rand.Int(rand.Reader, maxIdx)
		if err != nil {
			return "", err
		}
		b[i] = idChars[k.Int64()]
	}
	return string(b), nil
}

// slug turns an organization name into the start of its id: lower-case letters and digits separated by single
// dashes, at most maxSlug characters; "org" when nothing is left.
func slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		default:
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > maxSlug {
		s = strings.Trim(s[:maxSlug], "-")
	}
	if s == "" {
		return "org"
	}
	return s
}

// organizationID generates the id of a new organization: the slug of its name and a random suffix.
func organizationID(name string) (string, error) {
	suffix, err := randomString(idSuffixLen)
	if err != nil {
		return "", fmt.Errorf("signup: organization id: %w", err)
	}
	return slug(name) + "-" + suffix, nil
}

// newID is the id of a signup row.
func newID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("signup: id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
