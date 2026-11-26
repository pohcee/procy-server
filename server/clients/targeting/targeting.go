// Package targeting resolves the clients a job, schedule or upload is for: client IDs, group IDs or
// client tags, in a stable order, with the validation the API applies.
package targeting

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	errors2 "github.com/pohcee/procy-server/server/api/errors"
	"github.com/pohcee/procy-server/server/cgroups"
	"github.com/pohcee/procy-server/server/clients/clientdata"
	"github.com/pohcee/procy-server/share/models"
)

const (
	minClientsForTargeting = 1
)

var (
	ErrRequestIncludesMultipleTargetingParams = errors.New("multiple targeting options are not supported. Please specify only one")
	ErrRequestMissingTargetingParams          = errors.New("please specify targeting options, such as client ids, groups ids or tags")
	ErrMissingTagsInMultiJobRequest           = errors.New("please specify tags in the tags list")
)

// Params are the targeting options of a request: client IDs and group IDs, or client tags.
type Params interface {
	GetClientIDs() (ids []string)
	GetGroupIDs() (ids []string)
	GetClientTags() (clientTags *models.JobClientTags)
}

// ClientFinder finds clients in the tenant of ctx. clients.ClientService satisfies it.
type ClientFinder interface {
	GetByID(ctx context.Context, id string) (*clientdata.Client, error)
	GetByGroups(ctx context.Context, groups []*cgroups.ClientGroup) ([]*clientdata.Client, error)
	GetClientsByTag(ctx context.Context, tags []string, operator string, allowDisconnected bool) ([]*clientdata.Client, error)
}

// GroupFinder finds client groups in the tenant of ctx. cgroups.ClientGroupProvider satisfies it.
type GroupFinder interface {
	Get(ctx context.Context, id string) (*cgroups.ClientGroup, error)
}

// Resolver resolves targeting options to clients.
type Resolver struct {
	Clients ClientFinder
	Groups  GroupFinder
}

// Resolve validates the targeting options and returns the targeted clients (given client IDs first,
// then the clients of the given groups, without duplicates) and how many clients the groups had.
// Errors are errors.APIError with the HTTP status the API answers with.
func (r Resolver) Resolve(
	ctx context.Context,
	params Params,
) (targetedClients []*clientdata.Client, groupClientsCount int, err error) {
	err = checkParams(params)
	if err != nil {
		return nil, 0, err
	}

	if !HasClientTags(params) {
		// do the original client ids flow
		targetedClients, groupClientsCount, err = r.Ordered(ctx, params.GetClientIDs(), params.GetGroupIDs())
		if err != nil {
			return nil, 0, err
		}

		err := validateNonClientsTagTargeting(params, groupClientsCount, targetedClients)
		if err != nil {
			return nil, 0, err
		}
	} else {
		// do tags
		targetedClients, err = r.ByTag(ctx, params.GetClientTags())
		if err != nil {
			return nil, 0, err
		}

		err := validateClientTagsTargeting(targetedClients)
		if err != nil {
			return nil, 0, err
		}
	}
	return targetedClients, groupClientsCount, nil
}

// Ordered returns the clients with the given IDs, then the clients of the given groups that are not
// among them, and how many clients the groups had.
func (r Resolver) Ordered(
	ctx context.Context,
	clientIDs, groupIDs []string,
) (
	orderedClients []*clientdata.Client,
	groupClientsFoundCount int,
	err error,
) {
	groupClients, err := r.makeGroupClientsList(ctx, groupIDs)
	if err != nil {
		return nil, 0, err
	}
	groupClientsFoundCount = len(groupClients)

	orderedClients, usedClientIDs, err := r.makeClientsList(ctx, clientIDs)
	if err != nil {
		return orderedClients, groupClientsFoundCount, err
	}

	// append group clients
	for _, groupClient := range groupClients {
		groupClientID := groupClient.GetID()
		if !usedClientIDs[groupClientID] {
			usedClientIDs[groupClientID] = true
			orderedClients = append(orderedClients, groupClient)
		}
	}

	return orderedClients, groupClientsFoundCount, nil
}

func (r Resolver) makeGroupClientsList(ctx context.Context, groupIDs []string) (groupClients []*clientdata.Client, err error) {
	var groups []*cgroups.ClientGroup
	for _, groupID := range groupIDs {
		group, err := r.Groups.Get(ctx, groupID)
		if err != nil {
			err = errors2.APIError{
				Message:    fmt.Sprintf("Failed to get a client group with id=%q.", groupID),
				Err:        err,
				HTTPStatus: http.StatusInternalServerError,
			}
			return nil, err
		}
		if group == nil {
			err = errors2.APIError{
				Message:    fmt.Sprintf("Unknown group with id=%q.", groupID),
				Err:        err,
				HTTPStatus: http.StatusBadRequest,
			}
			return nil, err
		}
		groups = append(groups, group)
	}
	return r.Clients.GetByGroups(ctx, groups)
}

func (r Resolver) makeClientsList(ctx context.Context, clientIDs []string) (orderedClients []*clientdata.Client, usedClientIDs map[string]bool, err error) {
	orderedClients = make([]*clientdata.Client, 0)
	usedClientIDs = make(map[string]bool)

	for _, cid := range clientIDs {
		// TODO: doesn't GetByID exclude disconnected clients? if so, how can allowDisconnected work?
		client, err := r.Clients.GetByID(ctx, cid)
		if err != nil {
			err = errors2.APIError{
				Message:    fmt.Sprintf("Failed to find a client with id=%q.", cid),
				Err:        err,
				HTTPStatus: http.StatusInternalServerError,
			}
			return orderedClients, usedClientIDs, err
		}
		if client == nil {
			err = errors2.APIError{
				Message:    fmt.Sprintf("Client with id=%q not found.", cid),
				Err:        err,
				HTTPStatus: http.StatusNotFound,
			}
			return orderedClients, usedClientIDs, err
		}

		usedClientIDs[cid] = true
		orderedClients = append(orderedClients, client)
	}
	return orderedClients, usedClientIDs, nil
}

// ByTag returns the clients, connected or not, whose tags match.
func (r Resolver) ByTag(ctx context.Context, clientTags *models.JobClientTags) (
	orderedClients []*clientdata.Client,
	err error,
) {
	// find the clientIDs that have matching tags
	orderedClients, err = r.Clients.GetClientsByTag(ctx, clientTags.Tags, clientTags.Operator, true /* allowDisconnected */)
	if err != nil {
		err = errors2.APIError{
			Message:    "Unable to get active clients by tags",
			Err:        err,
			HTTPStatus: http.StatusInternalServerError,
		}
		return orderedClients, err
	}
	return orderedClients, err
}

func checkParams(params Params) (err error) {
	if params.GetClientIDs() == nil && params.GetGroupIDs() == nil && params.GetClientTags() == nil {
		return errors2.APIError{
			Message:    "Missing targeting parameters.",
			Err:        ErrRequestMissingTargetingParams,
			HTTPStatus: http.StatusBadRequest,
		}
	}
	if params.GetClientIDs() != nil && params.GetClientTags() != nil ||
		params.GetGroupIDs() != nil && params.GetClientTags() != nil {
		return errors2.APIError{
			Message:    "Multiple targeting parameters.",
			Err:        ErrRequestIncludesMultipleTargetingParams,
			HTTPStatus: http.StatusBadRequest,
		}
	}
	clientTags := params.GetClientTags()
	if clientTags != nil {
		if len(clientTags.Tags) == 0 {
			return errors2.APIError{
				Message:    "No tags specified.",
				Err:        ErrMissingTagsInMultiJobRequest,
				HTTPStatus: http.StatusBadRequest,
			}
		}
	}
	return nil
}

func validateNonClientsTagTargeting(params Params, groupClientsCount int, orderedClients []*clientdata.Client) (err error) {
	if len(params.GetGroupIDs()) > 0 && groupClientsCount == 0 && len(params.GetClientIDs()) == 0 {
		return errors2.APIError{
			Err:        errors.New("no active clients belong to the selected group(s)"),
			HTTPStatus: http.StatusBadRequest,
		}
	}

	if len(params.GetClientIDs()) < minClientsForTargeting && groupClientsCount == 0 {
		return errors2.APIError{
			Err:        errors.New("at least 1 client should be specified"),
			HTTPStatus: http.StatusBadRequest,
		}
	}

	if len(orderedClients) == 0 {
		return errors2.APIError{
			Err:        errors.New("at least 1 client should be specified"),
			HTTPStatus: http.StatusBadRequest,
		}
	}
	return nil
}

func validateClientTagsTargeting(orderedClients []*clientdata.Client) (err error) {
	if len(orderedClients) < minClientsForTargeting {
		return errors2.APIError{
			Err:        errors.New("at least 1 client should be specified"),
			HTTPStatus: http.StatusBadRequest,
		}
	}
	return nil
}

// HasClientTags reports whether params target clients by tags.
func HasClientTags(params Params) (has bool) {
	return params.GetClientTags() != nil
}
