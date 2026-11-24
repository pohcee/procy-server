package chserver

import (
	"context"

	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/server/clients/targeting"
	"github.com/openrport/openrport/share/models"
)

// The targeting names under their old names.
var (
	ErrRequestIncludesMultipleTargetingParams = targeting.ErrRequestIncludesMultipleTargetingParams
	ErrRequestMissingTargetingParams          = targeting.ErrRequestMissingTargetingParams
	ErrMissingTagsInMultiJobRequest           = targeting.ErrMissingTagsInMultiJobRequest
)

type TargetingParams = targeting.Params

// targets resolves targeting options with the API's client service and client groups.
func (al *APIListener) targets() targeting.Resolver {
	return targeting.Resolver{Clients: al.clientService, Groups: al.clientGroupProvider}
}

func (al *APIListener) getOrderedClientsWithValidation(ctx context.Context, params TargetingParams) ([]*clientdata.Client, int, error) {
	return al.targets().Resolve(ctx, params)
}

func (al *APIListener) getOrderedClients(ctx context.Context, clientIDs, groupIDs []string) ([]*clientdata.Client, int, error) {
	return al.targets().Ordered(ctx, clientIDs, groupIDs)
}

func (al *APIListener) getOrderedClientsByTag(ctx context.Context, clientTags *models.JobClientTags) ([]*clientdata.Client, error) {
	return al.targets().ByTag(ctx, clientTags)
}

func hasClientTags(params TargetingParams) bool {
	return targeting.HasClientTags(params)
}
