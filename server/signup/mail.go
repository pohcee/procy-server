package signup

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/pohcee/procy-server/server/notifications/channels/rmailer"
	"github.com/pohcee/procy-server/share/logger"
)

// Paths of the web UI the links in the emails open (below [signup] public_url). The UI reads the token from the
// query and posts it to the API.
const (
	VerifyPath         = "/auth/signup/verify"
	CancelDeletionPath = "/auth/signup/cancel-deletion"
	ConfirmEmailPath   = "/auth/confirm-email"
	// SignInPath is the web UI's sign-in page.
	SignInPath = "/auth"
	// OperatorQueuePath is the operator console's signup queue (the "New signup waiting" email links it).
	OperatorQueuePath = "/operator/signups"
)

// SMTPMailer sends through the server's [smtp] settings, the mailer of the email notifications.
type SMTPMailer struct{ M rmailer.Mailer }

// Send implements Mailer.
func (s SMTPMailer) Send(ctx context.Context, to, subject, body string) error {
	return s.M.Send(ctx, []string{to}, subject, rmailer.ContentTypeTextPlain, body)
}

// LogMailer writes the emails to the log instead of sending them ([signup] mail = "log", for development).
type LogMailer struct{ Log *logger.Logger }

// Send implements Mailer.
func (l LogMailer) Send(_ context.Context, to, subject, body string) error {
	l.Log.Infof("signup email (not sent, [signup] mail = \"log\") to %s: %s\n%s", to, subject, body)
	return nil
}

func link(base, path, token string) string {
	return base + path + "?token=" + url.QueryEscape(token)
}

type mail struct{ subject, body string }

func verifyMail(base, token, org string, ttl time.Duration, takeOver bool) mail {
	extra := ""
	if takeOver {
		extra = `
An administrator of an organization created an account with this address, and nobody has confirmed it. Continuing
makes it yours: you choose its password, its sessions end, and the organizations it was added to become
invitations you can accept or decline.
`
	}
	return mail{
		subject: "Confirm your email address",
		body: fmt.Sprintf(`Hello,

someone (hopefully you) asked to create the organization %q with this email address.
To confirm, choose your password and create it, open this link within %s:

%s
%s
If you didn't ask for this, ignore this email: nothing is created without the link.
`, org, ttl.Round(time.Minute), link(base, VerifyPath, token), extra),
	}
}

func confirmEmailMail(base, token string, ttl time.Duration) mail {
	return mail{
		subject: "Confirm your email address",
		body: fmt.Sprintf(`Hello,

the account with this address asked to confirm it. Sign in to that account and open this link within %s:

%s

If you don't have an account with this address, ignore this email: nobody else can use the link. To make an
account someone else created for this address yours, sign up with this address instead.
`, ttl.Round(time.Minute), link(base, ConfirmEmailPath, token)),
	}
}

func existingMail(base string) mail {
	return mail{
		subject: "You already have an account",
		body: fmt.Sprintf(`Hello,

someone (hopefully you) asked to sign up with this email address, but it already has an account.
Sign in at %s instead. If you forgot your password, ask an administrator of your organization.

If you didn't ask for this, ignore this email.
`, base),
	}
}

func readyMail(base, org, orgID, method string) mail {
	how := "with this email address and the password you chose"
	if method == MethodGoogle {
		how = `with "Sign in with Google", using the Google account of this address`
	}
	return mail{
		subject: "Your organization " + org + " is ready",
		body: fmt.Sprintf(`Hello,

you're in: your organization %q (id %s) was created. Sign in %s:

%s
`, org, orgID, how, base+SignInPath),
	}
}

func deniedMail(org, reason string) mail {
	why := ""
	if reason = strings.TrimSpace(reason); reason != "" {
		why = "\nThe reason given: " + reason + "\n"
	}
	return mail{
		subject: "Your signup for " + org,
		body: fmt.Sprintf(`Hello,

your request to create the organization %q was not approved.
%s
If you think this is a mistake, reply to this email.
`, org, why),
	}
}

// notifyMail tells the operators ([signup] notify_email) that a signup waits for approval.
func notifyMail(base string, r *Signup, freeMail bool) mail {
	mx := "not checked"
	if r.MXFound != nil {
		mx = map[bool]string{true: "yes", false: "NO"}[*r.MXFound]
	}
	proven := "the emailed link"
	if r.Method == MethodGoogle {
		proven = "Google sign-in"
	}
	kind := ""
	if freeMail {
		kind = " (free mail)"
	}
	return mail{
		subject: fmt.Sprintf("New signup waiting: %q", r.OrgName),
		body: fmt.Sprintf(`A signup is waiting for approval.

Organization: %q
Email:        %s
Domain:       %s%s
Mail server:  %s
Proven by:    %s
From IP:      %s

Approve or reject it at %s
`, r.OrgName, r.Email, r.Domain, kind, mx, proven, r.RemoteIP, base+OperatorQueuePath),
	}
}

func awaitingMail(org string) mail {
	return mail{
		subject: "Your signup for " + org + " is waiting for approval",
		body: fmt.Sprintf(`Hello,

thank you for confirming your email. Every new organization is reviewed by a person: %q will be created once it
is approved, and you'll get an email then.
`, org),
	}
}

func deletionMail(base, org, orgID, token string, at time.Time) mail {
	return mail{
		subject: "Your organization " + org + " will be deleted",
		body: fmt.Sprintf(`Hello,

the deletion of the organization %q (id %s) was requested. It is suspended now: nobody can sign in and devices
can't connect. On %s its data will be deleted for good.

To keep the organization, open this link before then:

%s
`, org, orgID, at.UTC().Format("2006-01-02 15:04 MST"), link(base, CancelDeletionPath, token)),
	}
}

func deletionNotice(org, orgID string, at time.Time) mail {
	return mail{
		subject: "Your organization " + org + " will be deleted",
		body: fmt.Sprintf(`Hello,

a platform operator started the deletion of the organization %q (id %s). It is suspended now: nobody can sign in
and devices can't connect. On %s its data will be deleted for good. Contact the operators of this service if
this is a mistake.
`, org, orgID, at.UTC().Format("2006-01-02 15:04 MST")),
	}
}
