package api

import (
	"context"

	"ottstreamer/internal/apikey"
)

func (s *server) GetMe(ctx context.Context, _ GetMeRequestObject) (GetMeResponseObject, error) {
	return GetMe200JSONResponse{Name: apikey.Name(ctx), Role: MeRole(apikey.Role(ctx))}, nil
}
