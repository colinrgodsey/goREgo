package server

import (
	"context"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/colinrgodsey/goREgo/lib/storage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type ContentAddressableStorageServer struct {
	repb.UnimplementedContentAddressableStorageServer
	Store storage.BlobStore
}

func NewContentAddressableStorageServer(store storage.BlobStore) *ContentAddressableStorageServer {
	return &ContentAddressableStorageServer{Store: store}
}

func (s *ContentAddressableStorageServer) FindMissingBlobs(ctx context.Context, req *repb.FindMissingBlobsRequest) (*repb.FindMissingBlobsResponse, error) {
	resp := &repb.FindMissingBlobsResponse{}
	for _, d := range req.BlobDigests {
		digest, err := storage.FromProto(d)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid digest: %v", err)
		}
		exists, err := s.Store.Has(ctx, digest)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "storage error: %v", err)
		}
		if !exists {
			resp.MissingBlobDigests = append(resp.MissingBlobDigests, d)
		}
	}
	return resp, nil
}

func (s *ContentAddressableStorageServer) BatchUpdateBlobs(ctx context.Context, req *repb.BatchUpdateBlobsRequest) (*repb.BatchUpdateBlobsResponse, error) {
	// TODO: Implement batch update (requires reading from request)
	// This is slightly complex because req.Requests contains inline data.
	return &repb.BatchUpdateBlobsResponse{}, nil
}

func (s *ContentAddressableStorageServer) GetTree(req *repb.GetTreeRequest, stream repb.ContentAddressableStorage_GetTreeServer) error {
	return status.Error(codes.Unimplemented, "GetTree not implemented")
}

type CapabilitiesServer struct {
	repb.UnimplementedCapabilitiesServer
}

func NewCapabilitiesServer() *CapabilitiesServer {
	return &CapabilitiesServer{}
}

func (s *CapabilitiesServer) GetCapabilities(ctx context.Context, req *repb.GetCapabilitiesRequest) (*repb.ServerCapabilities, error) {
	return &repb.ServerCapabilities{
		CacheCapabilities: &repb.CacheCapabilities{
			DigestFunctions: []repb.DigestFunction_Value{
				repb.DigestFunction_SHA256,
			},
			ActionCacheUpdateCapabilities: &repb.ActionCacheUpdateCapabilities{
				UpdateEnabled: true,
			},
		},
		ExecutionCapabilities: &repb.ExecutionCapabilities{
			DigestFunction: repb.DigestFunction_SHA256,
			ExecEnabled:    false,
		},
	}, nil
}
