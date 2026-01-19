package server

import (
	"context"
	"os"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/colinrgodsey/goREgo/pkg/storage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type ActionCacheServer struct {
	repb.UnimplementedActionCacheServer
	Store storage.ActionCache
}

func NewActionCacheServer(store storage.ActionCache) *ActionCacheServer {
	return &ActionCacheServer{Store: store}
}

func (s *ActionCacheServer) GetActionResult(ctx context.Context, req *repb.GetActionResultRequest) (*repb.ActionResult, error) {
	dg, err := storage.FromProto(req.ActionDigest)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid digest: %v", err)
	}

	res, err := s.Store.GetActionResult(ctx, dg)
	if err != nil {
		if os.IsNotExist(err) || status.Code(err) == codes.NotFound {
			return nil, status.Errorf(codes.NotFound, "action result not found: %v", dg)
		}
		return nil, status.Errorf(codes.Internal, "failed to get action result: %v", err)
	}

	return res, nil
}

func (s *ActionCacheServer) UpdateActionResult(ctx context.Context, req *repb.UpdateActionResultRequest) (*repb.ActionResult, error) {
	dg, err := storage.FromProto(req.ActionDigest)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid digest: %v", err)
	}

	if err := s.Store.UpdateActionResult(ctx, dg, req.ActionResult); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to update action result: %v", err)
	}

	return req.ActionResult, nil
}
