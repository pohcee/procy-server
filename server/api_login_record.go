package chserver

import (
	"context"
	"net/http"

	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/server/auditlog"
	"github.com/openrport/openrport/share/logger"
)

// LoginRecorder records full sign-ins: when a member last signed in to its organization (Postgres, the operator
// console). nil: not recorded (standalone).
type LoginRecorder interface {
	// RecordLogin records that username signed in to the tenant of ctx now.
	RecordLogin(ctx context.Context, username string) error
}

// auditApplicationLogin is the audit log application of password sign-ins.
const auditApplicationLogin = "auth.login"

// loginSucceeded records a full sign-in of username (a token with every scope: a password without 2FA, after 2FA,
// OIDC), never the 2FA-pending token, an organization switch or an API token. A failure is logged: the sign-in
// itself succeeded.
func (al *APIListener) loginSucceeded(ctx context.Context, username string) {
	if al.loginRecorder == nil {
		return
	}
	if err := al.loginRecorder.RecordLogin(ctx, username); err != nil {
		al.Logf(logger.LogLevelError, "%v", err)
	}
}

// auditLogin writes a password sign-in of username to the audit log (action success or failed). Only metadata: the
// login, the address and the outcome. Only where sign-ins are recorded (Postgres): standalone audit logs stay as
// they were.
func (al *APIListener) auditLogin(req *http.Request, username, action string) {
	if al.loginRecorder == nil {
		return
	}
	al.auditLog.Entry(auditApplicationLogin, action).WithHTTPRequest(req.WithContext(api.WithUser(req.Context(), username))).WithID(username).Save()
}

// passwordLoginSucceeded records and audits a full password sign-in.
func (al *APIListener) passwordLoginSucceeded(req *http.Request, username string) {
	al.loginSucceeded(req.Context(), username)
	al.auditLogin(req, username, auditlog.ActionSuccess)
}
