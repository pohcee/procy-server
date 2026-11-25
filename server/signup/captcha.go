package signup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/openrport/openrport/server/chconfig"
)

// Server-side verification endpoints of the captcha providers.
const (
	TurnstileVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	HCaptchaVerifyURL  = "https://api.hcaptcha.com/siteverify"
)

const captchaTimeout = 10 * time.Second

// SiteVerify checks captcha responses with a provider's siteverify endpoint (Turnstile and hCaptcha share the
// protocol: a form POST of secret, response and remoteip, answered with JSON {"success": bool}). It fails closed:
// any error refuses the signup.
type SiteVerify struct {
	URL    string
	Secret string
	Client *http.Client
}

// NewCaptcha returns the captcha of [signup] captcha ("" → nil), its secret read from captcha_secret_file.
func NewCaptcha(cfg chconfig.SignupConfig) (Captcha, error) {
	var u string
	switch cfg.Captcha {
	case "":
		return nil, nil
	case chconfig.SignupCaptchaTurnstile:
		u = TurnstileVerifyURL
	case chconfig.SignupCaptchaHCaptcha:
		u = HCaptchaVerifyURL
	default:
		return nil, fmt.Errorf("signup: unknown captcha %q", cfg.Captcha)
	}
	b, err := os.ReadFile(cfg.CaptchaSecretFile)
	if err != nil {
		return nil, fmt.Errorf("signup: captcha secret: %w", err)
	}
	secret := strings.TrimSpace(string(b))
	if secret == "" {
		return nil, fmt.Errorf("signup: the captcha secret file %s is empty", cfg.CaptchaSecretFile)
	}
	return &SiteVerify{URL: u, Secret: secret, Client: &http.Client{Timeout: captchaTimeout}}, nil
}

// Verify implements Captcha.
func (s *SiteVerify) Verify(ctx context.Context, response, remoteIP string) error {
	if strings.TrimSpace(response) == "" {
		return ErrCaptcha
	}
	form := url.Values{"secret": {s.Secret}, "response": {response}}
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCaptcha, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c := s.Client
	if c == nil {
		c = &http.Client{Timeout: captchaTimeout}
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("%w: the provider could not be reached", ErrCaptcha)
	}
	defer resp.Body.Close()
	var out struct {
		Success bool `json:"success"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out) != nil || !out.Success {
		return ErrCaptcha
	}
	return nil
}
