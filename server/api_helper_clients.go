package chserver

import (
	"context"

	"github.com/pohcee/procy-server/server/clients/clientdata"
	"github.com/pohcee/procy-server/server/clients/targeting"
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
