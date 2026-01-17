package server

import (
	"bytes"
	"context"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/bazelbuild/remote-apis/build/bazel/semver"
	"github.com/colinrgodsey/goREgo/pkg/storage"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	gstatus "google.golang.org/grpc/status"
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
			return nil, gstatus.Errorf(codes.InvalidArgument, "invalid digest: %v", err)
		}
		exists, err := s.Store.Has(ctx, digest)
		if err != nil {
			return nil, gstatus.Errorf(codes.Internal, "storage error: %v", err)
		}
		if !exists {
			resp.MissingBlobDigests = append(resp.MissingBlobDigests, d)
		}
	}
	return resp, nil
}

func (s *ContentAddressableStorageServer) BatchUpdateBlobs(ctx context.Context, req *repb.BatchUpdateBlobsRequest) (*repb.BatchUpdateBlobsResponse, error) {
	resp := &repb.BatchUpdateBlobsResponse{}
	for _, r := range req.Requests {
		dg, err := storage.FromProto(r.Digest)
		if err != nil {
			resp.Responses = append(resp.Responses, &repb.BatchUpdateBlobsResponse_Response{
				Digest: r.Digest,
				Status: &status.Status{Code: int32(codes.InvalidArgument), Message: err.Error()},
			})
			continue
		}

		err = s.Store.Put(ctx, dg, bytes.NewReader(r.Data))
		if err != nil {
			resp.Responses = append(resp.Responses, &repb.BatchUpdateBlobsResponse_Response{
				Digest: r.Digest,
				Status: &status.Status{Code: int32(codes.Internal), Message: err.Error()},
			})
			continue
		}

		resp.Responses = append(resp.Responses, &repb.BatchUpdateBlobsResponse_Response{
			Digest: r.Digest,
			Status: &status.Status{Code: int32(codes.OK)},
		})
	}
	return resp, nil
}

func (s *ContentAddressableStorageServer) GetTree(req *repb.GetTreeRequest, stream repb.ContentAddressableStorage_GetTreeServer) error {
	return gstatus.Error(codes.Unimplemented, "GetTree not implemented")
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
			MaxBatchTotalSizeBytes: 4 * 1024 * 1024,
		},
		ExecutionCapabilities: &repb.ExecutionCapabilities{
			DigestFunction: repb.DigestFunction_SHA256,
			ExecEnabled:    false,
		},
		LowApiVersion:  &semver.SemVer{Major: 2, Minor: 0},
		HighApiVersion: &semver.SemVer{Major: 2, Minor: 2},
	}, nil
}
