package signup

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/openrport/openrport/server/notifications/channels/rmailer"
	"github.com/openrport/openrport/share/logger"
)

// Paths of the web UI the links in the emails open (below [signup] public_url). The UI reads the token from the
// query and posts it to the API.
const (
	VerifyPath         = "/auth/signup/verify"
	CancelDeletionPath = "/auth/signup/cancel-deletion"
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

func verifyMail(base, token, org string, ttl time.Duration) mail {
	return mail{
		subject: "Confirm your email address",
		body: fmt.Sprintf(`Hello,

someone (hopefully you) asked to create the organization %q with this email address.
To confirm and create it, open this link within %s:

%s

If you didn't ask for this, ignore this email: nothing is created without the link.
`, org, ttl.Round(time.Minute), link(base, VerifyPath, token)),
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

func readyMail(base, org, orgID string) mail {
	return mail{
		subject: "Your organization " + org + " is ready",
		body: fmt.Sprintf(`Hello,

your organization %q (id %s) was created. Sign in at %s with this email address and the password you chose.
`, org, orgID, base),
	}
}

func deniedMail(org string) mail {
	return mail{
		subject: "Your signup for " + org,
		body: fmt.Sprintf(`Hello,

your request to create the organization %q was not approved.
`, org),
	}
}

func awaitingMail(org string) mail {
	return mail{
		subject: "Your signup for " + org + " is waiting for approval",
		body: fmt.Sprintf(`Hello,

thank you for confirming your email. The organization %q will be created once an operator approves it; you'll get
an email then.
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
