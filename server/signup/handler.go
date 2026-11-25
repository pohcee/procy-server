package signup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"

	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/operator"
	chshare "github.com/openrport/openrport/share"
	"github.com/openrport/openrport/share/tenant"
)

const maxBody = 16 << 10

// Handler serves the signup API.
type Handler struct {
	svc         *Service
	respond     *api.Responder
	currentUser func(ctx context.Context) (*users.User, error)
}

// NewHandler returns the signup API handler. currentUser returns the authenticated user of a request.
func NewHandler(svc *Service, respond *api.Responder, currentUser func(ctx context.Context) (*users.User, error)) *Handler {
	return &Handler{svc: svc, respond: respond, currentUser: currentUser}
}

// MountPublic registers the unauthenticated routes on r (below routes.AllRoutesPrefix).
func (h *Handler) MountPublic(r *mux.Router) {
	r.HandleFunc("/signup", h.config).Methods(http.MethodGet)
	r.HandleFunc("/signup", h.request).Methods(http.MethodPost)
	r.HandleFunc("/signup/verify", h.verify).Methods(http.MethodPost)
	r.HandleFunc("/signup/cancel-deletion", h.cancelByToken).Methods(http.MethodPost)
}

// MountAccount registers the routes of a signed-in account on r, a router behind authentication.
func (h *Handler) MountAccount(r *mux.Router) {
	r.HandleFunc("/me/email-confirmation", h.requestEmailConfirmation).Methods(http.MethodPost)
	r.HandleFunc("/me/email-confirmation/confirm", h.confirmEmail).Methods(http.MethodPost)
}

func (h *Handler) requestEmailConfirmation(w http.ResponseWriter, r *http.Request) {
	u, err := h.currentUser(r.Context())
	if err != nil {
		h.respond.Error(w, err)
		return
	}
	st, err := h.svc.RequestEmailConfirmation(r.Context(), u.Username, chshare.RemoteIP(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(st))
}

func (h *Handler) confirmEmail(w http.ResponseWriter, r *http.Request) {
	u, err := h.currentUser(r.Context())
	if err != nil {
		h.respond.Error(w, err)
		return
	}
	var in tokenRequest
	if !h.decode(w, r, &in) {
		return
	}
	st, err := h.svc.ConfirmEmail(r.Context(), u.Username, in.Token, chshare.RemoteIP(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(st))
}

// MountOrganization registers the routes of an organization's administrators on r, a router behind authentication;
// admin is the middleware that lets only administrators through.
func (h *Handler) MountOrganization(r *mux.Router, admin mux.MiddlewareFunc) {
	r.Handle("/organization/export", admin(http.HandlerFunc(h.exportOwn))).Methods(http.MethodGet)
	r.Handle("/organization/deletion", admin(http.HandlerFunc(h.deleteOwn))).Methods(http.MethodPost)
}

// MountOperator registers the operator routes on r, the operator API router (operators only).
func (h *Handler) MountOperator(r *mux.Router) {
	r.HandleFunc("/signups", h.list).Methods(http.MethodGet)
	r.HandleFunc("/signups/{id}/approve", h.approve).Methods(http.MethodPost)
	r.HandleFunc("/signups/{id}/deny", h.deny).Methods(http.MethodPost)
	r.HandleFunc("/deletions", h.listDeletions).Methods(http.MethodGet)
	r.HandleFunc("/organizations/{id}/export", h.exportAny).Methods(http.MethodGet)
	r.HandleFunc("/organizations/{id}/deletion", h.deleteAny).Methods(http.MethodPost)
	r.HandleFunc("/organizations/{id}/deletion", h.cancel).Methods(http.MethodDelete)
	r.HandleFunc("/organizations/{id}/deletion/expedite", h.expedite).Methods(http.MethodPost)
}

func (h *Handler) config(w http.ResponseWriter, _ *http.Request) {
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(h.svc.PublicConfig()))
}

func (h *Handler) request(w http.ResponseWriter, r *http.Request) {
	var in Request
	if !h.decode(w, r, &in) {
		return
	}
	if err := h.svc.Request(r.Context(), in, chshare.RemoteIP(r)); err != nil {
		h.fail(w, err)
		return
	}
	h.respond.JSON(w, http.StatusAccepted, api.NewSuccessPayload(map[string]string{
		"status": "sent", "message": "If the address can sign up, a link to continue was sent to it.",
	}))
}

type tokenRequest struct {
	Token string `json:"token"`
}

type verifyRequest struct {
	Token string `json:"token"`
	// Password is the password the new administrator chooses.
	Password string `json:"password"`
}

func (h *Handler) verify(w http.ResponseWriter, r *http.Request) {
	var in verifyRequest
	if !h.decode(w, r, &in) {
		return
	}
	res, err := h.svc.Verify(r.Context(), in.Token, in.Password, chshare.RemoteIP(r))
	if err != nil {
		// distinct codes for the two refusals the page tells apart, with the same status as ever (400)
		switch {
		case errors.Is(err, ErrPassword):
			h.respond.ErrorWithErrCode(w, http.StatusBadRequest, ErrCodeInvalidPassword, err.Error())
			return
		case errors.Is(err, ErrToken):
			h.respond.ErrorWithErrCode(w, http.StatusBadRequest, ErrCodeSignupLinkInvalid, err.Error())
			return
		}
		h.fail(w, err)
		return
	}
	code := http.StatusCreated
	if res.Status == StatusAwaitingApproval {
		code = http.StatusAccepted
	}
	h.respond.JSON(w, code, api.NewSuccessPayload(res))
}

// EmailVerified reports whether the owner of the mailbox named by login proved it (GET /me); ok is false when the
// account is unknown or can't be read.
func (h *Handler) EmailVerified(ctx context.Context, login string) (verified, ok bool) {
	a, err := h.svc.o.Accounts.Account(ctx, login)
	if err != nil || !a.Exists {
		return false, false
	}
	return a.Verified, true
}

func (h *Handler) cancelByToken(w http.ResponseWriter, r *http.Request) {
	var in tokenRequest
	if !h.decode(w, r, &in) {
		return
	}
	d, err := h.svc.CancelDeletionByToken(r.Context(), in.Token, chshare.RemoteIP(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(map[string]string{"organization": d.OrganizationID, "state": d.State}))
}

// own returns the organization of the request and its administrator as the actor.
func (h *Handler) own(w http.ResponseWriter, r *http.Request) (string, operator.Actor, bool) {
	tid, err := tenant.FromContext(r.Context())
	if err != nil {
		h.respond.Error(w, err)
		return "", operator.Actor{}, false
	}
	u, err := h.currentUser(r.Context())
	if err != nil {
		h.respond.Error(w, err)
		return "", operator.Actor{}, false
	}
	return string(tid), operator.Actor{Username: u.Username, RemoteIP: chshare.RemoteIP(r)}, true
}

func (h *Handler) exportOwn(w http.ResponseWriter, r *http.Request) {
	org, by, ok := h.own(w, r)
	if ok {
		h.export(w, r, by, org)
	}
}

func (h *Handler) exportAny(w http.ResponseWriter, r *http.Request) {
	h.export(w, r, operator.ActorOf(r), mux.Vars(r)["id"])
}

func (h *Handler) export(w http.ResponseWriter, r *http.Request, by operator.Actor, org string) {
	e, err := h.svc.Export(r.Context(), by, org)
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", org+"-export.json"))
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(e))
}

type deletionRequest struct {
	// Confirm must be the organization's id.
	Confirm string `json:"confirm"`
	Reason  string `json:"reason"`
}

func (h *Handler) deleteOwn(w http.ResponseWriter, r *http.Request) {
	org, by, ok := h.own(w, r)
	if !ok {
		return
	}
	var in deletionRequest
	if !h.decode(w, r, &in) {
		return
	}
	if in.Confirm != org {
		h.fail(w, fmt.Errorf("%w: confirm must be the organization's id (%s)", ErrInvalid, org))
		return
	}
	h.startDeletion(w, r, by, org, in.Reason, false)
}

func (h *Handler) deleteAny(w http.ResponseWriter, r *http.Request) {
	var in deletionRequest
	if !h.decode(w, r, &in) {
		return
	}
	h.startDeletion(w, r, operator.ActorOf(r), mux.Vars(r)["id"], in.Reason, true)
}

func (h *Handler) startDeletion(w http.ResponseWriter, r *http.Request, by operator.Actor, org, reason string, byOperator bool) {
	d, err := h.svc.RequestDeletion(r.Context(), by, org, reason, byOperator)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.respond.JSON(w, http.StatusAccepted, api.NewSuccessPayload(d))
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	d, err := h.svc.CancelDeletion(r.Context(), operator.ActorOf(r), mux.Vars(r)["id"])
	if err != nil {
		h.fail(w, err)
		return
	}
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(d))
}

func (h *Handler) expedite(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Expedite(r.Context(), operator.ActorOf(r), mux.Vars(r)["id"]); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func limitParam(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	return n
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.List(r.Context(), r.URL.Query().Get("status"), limitParam(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(out))
}

func (h *Handler) listDeletions(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.ListDeletions(r.Context(), r.URL.Query().Get("state"), limitParam(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(out))
}

func (h *Handler) approve(w http.ResponseWriter, r *http.Request) {
	res, err := h.svc.Approve(r.Context(), operator.ActorOf(r), mux.Vars(r)["id"])
	if err != nil {
		h.fail(w, err)
		return
	}
	h.respond.JSON(w, http.StatusCreated, api.NewSuccessPayload(res))
}

type denyRequest struct {
	Reason string `json:"reason"`
}

func (h *Handler) deny(w http.ResponseWriter, r *http.Request) {
	var in denyRequest
	if r.ContentLength != 0 && !h.decode(w, r, &in) {
		return
	}
	if len(in.Reason) > maxReason {
		h.fail(w, fmt.Errorf("%w: the reason must be at most %d characters", ErrInvalid, maxReason))
		return
	}
	if err := h.svc.Deny(r.Context(), operator.ActorOf(r), mux.Vars(r)["id"], in.Reason); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		h.fail(w, fmt.Errorf("%w: %v", ErrInvalid, err))
		return false
	}
	return true
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	status := 0
	switch {
	case errors.Is(err, ErrInvalid), errors.Is(err, ErrCaptcha), errors.Is(err, ErrDomain), errors.Is(err, ErrToken):
		status = http.StatusBadRequest
	case errors.Is(err, ErrRateLimited):
		status = http.StatusTooManyRequests
	case errors.Is(err, ErrNotFound), errors.Is(err, operator.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrConflict), errors.Is(err, operator.ErrNotActive), errors.Is(err, operator.ErrExists):
		status = http.StatusConflict
	case errors.Is(err, ErrMail):
		status = http.StatusServiceUnavailable
	}
	if status != 0 {
		h.respond.ErrorWithTitle(w, status, err.Error())
		return
	}
	h.respond.Error(w, err)
}
