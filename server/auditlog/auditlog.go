package auditlog

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/auditlog/config"

	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/query"
)

var (
	supportedFilters = map[string]bool{
		"timestamp[gt]":    true,
		"timestamp[lt]":    true,
		"timestamp[since]": true,
		"timestamp[until]": true,
		"username":         true,
		"remote_ip":        true,
		"application":      true,
		"action":           true,
		"affected_id":      true,
		"client_id":        true,
		"client_hostname":  true,
	}
	supportedSorts = map[string]bool{
		"timestamp":       true,
		"username":        true,
		"remote_ip":       true,
		"application":     true,
		"action":          true,
		"affected_id":     true,
		"client_id":       true,
		"client_hostname": true,
	}
)

// ClientGetter looks up a client in the tenant carried by ctx.
type ClientGetter interface {
	GetByID(ctx context.Context, id string) (*clientdata.Client, error)
}

// Provider stores audit log entries. Save, List and Count are scoped to the tenant carried by their context and fail
// with tenant.ErrMissing when there is none.
type Provider interface {
	io.Closer
	// Save stores the entry for the tenant carried by ctx.
	Save(ctx context.Context, e *Entry) error
	List(context.Context, *query.ListOptions) ([]*Entry, error)
	Count(context.Context, *query.ListOptions) (int, error)
}

type AuditLog struct {
	logger       *logger.Logger
	clientGetter ClientGetter
	provider     Provider
	config       config.Config
}

type NotAllowedError struct {
	Msg string
}

func (e *NotAllowedError) Error() string {
	return e.Msg
}

// New creates the audit log. p stores the entries; it is used only when the audit log is enabled and may be nil
// otherwise.
func New(_ context.Context, l *logger.Logger, cg ClientGetter, cfg config.Config, p Provider) (*AuditLog, error) {
	a := &AuditLog{
		logger:       l,
		clientGetter: cg,
		config:       cfg,
	}

	if cfg.Enable {
		if p == nil {
			return nil, errors.New("auditlog: enabled without a provider")
		}
		a.provider = p
	}

	return a, nil
}

func (a *AuditLog) Entry(application, action string) *Entry {
	// return nil if auditlog is not initialized, Entry handles nils so we don't panic unnecessarily
	if a == nil || !a.config.Enable {
		return nil
	}

	e := &Entry{
		Timestamp:   time.Now(),
		Application: application,
		Action:      action,

		al: a,
	}

	return e
}

func (a *AuditLog) Close() error {
	if a == nil || a.provider == nil {
		return nil
	}

	return a.provider.Close()
}

func (a *AuditLog) savePreparedEntry(e *Entry) error {
	if a.provider == nil {
		return nil
	}

	if a.config.UseIPObfuscation && e.RemoteIP != "" {
		ip := net.ParseIP(e.RemoteIP)
		if ip.To4() != nil {
			e.RemoteIP = strings.TrimSuffix(ip.Mask(net.CIDRMask(24, 32)).String(), "0") + "x"
		}
	}

	// the request may be over by the time the entry is saved: keep its tenant, drop its cancellation
	return a.provider.Save(context.WithoutCancel(e.context()), e)
}

func (a *AuditLog) List(r *http.Request, user *users.User) (*api.SuccessPayload, error) {
	options := query.GetListOptions(r)
	if !user.IsAdmin() {
		// Deny none-admins looking for foreign audit logs
		for _, v := range options.Filters {
			for _, col := range v.Column {
				if col == "username" {
					return nil, &NotAllowedError{"only members of group Administrators can filter by usernames"}
				}
			}
		}
		// Add a forced filter so none-admins cannot inspect what others have done
		options.Filters = append(options.Filters, query.FilterOption{
			Column: []string{"username"},
			Values: []string{user.Username},
		})
	}
	err := query.ValidateListOptions(options, supportedSorts, supportedFilters, nil, &query.PaginationConfig{
		DefaultLimit: 10,
		MaxLimit:     100,
	})
	if err != nil {
		return nil, err
	}

	entries, err := a.provider.List(r.Context(), options)
	if err != nil {
		return nil, err
	}

	count, err := a.provider.Count(r.Context(), options)
	if err != nil {
		return nil, err
	}

	return &api.SuccessPayload{
		Data: entries,
		Meta: api.NewMeta(count),
	}, nil
}
