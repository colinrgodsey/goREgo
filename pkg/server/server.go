package server

import (
	"bytes"
	"context"
	"log/slog"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/bazelbuild/remote-apis/build/bazel/semver"
	"github.com/colinrgodsey/goREgo/pkg/storage"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	gstatus "google.golang.org/grpc/status"
)

type ContentAddressableStorageServer struct {
	repb.UnimplementedContentAddressableStorageServer
	Store  storage.BlobStore
	logger *slog.Logger
}

func NewContentAddressableStorageServer(store storage.BlobStore) *ContentAddressableStorageServer {
	return &ContentAddressableStorageServer{
		Store:  store,
		logger: slog.Default().With("component", "cas"),
	}
}

func (s *ContentAddressableStorageServer) FindMissingBlobs(ctx context.Context, req *repb.FindMissingBlobsRequest) (*repb.FindMissingBlobsResponse, error) {
	resp := &repb.FindMissingBlobsResponse{}
	for _, d := range req.BlobDigests {
		digest, err := storage.FromProto(d)
		if err != nil {
			s.logger.Error("invalid digest in FindMissingBlobs", "hash", d.Hash, "error", err)
			return nil, gstatus.Errorf(codes.InvalidArgument, "invalid digest: %v", err)
		}
		exists, err := s.Store.Has(ctx, digest)
		if err != nil {
			s.logger.Error("storage error in FindMissingBlobs", "hash", digest.Hash, "size", digest.Size, "error", err)
			return nil, gstatus.Errorf(codes.Internal, "storage error: %v", err)
		}
		if !exists {
			resp.MissingBlobDigests = append(resp.MissingBlobDigests, d)
		}
	}
	s.logger.Debug("FindMissingBlobs", "requested", len(req.BlobDigests), "missing", len(resp.MissingBlobDigests))
	return resp, nil
}

func (s *ContentAddressableStorageServer) BatchUpdateBlobs(ctx context.Context, req *repb.BatchUpdateBlobsRequest) (*repb.BatchUpdateBlobsResponse, error) {
	resp := &repb.BatchUpdateBlobsResponse{}
	var successCount, errorCount int
	for _, r := range req.Requests {
		dg, err := storage.FromProto(r.Digest)
		if err != nil {
			s.logger.Error("invalid digest in BatchUpdateBlobs", "hash", r.Digest.Hash, "error", err)
			resp.Responses = append(resp.Responses, &repb.BatchUpdateBlobsResponse_Response{
				Digest: r.Digest,
				Status: &status.Status{Code: int32(codes.InvalidArgument), Message: err.Error()},
			})
			errorCount++
			continue
		}

		err = s.Store.Put(ctx, dg, bytes.NewReader(r.Data))
		if err != nil {
			s.logger.Error("store put failed in BatchUpdateBlobs", "hash", dg.Hash, "size", dg.Size, "dataLen", len(r.Data), "error", err)
			resp.Responses = append(resp.Responses, &repb.BatchUpdateBlobsResponse_Response{
				Digest: r.Digest,
				Status: &status.Status{Code: int32(codes.Internal), Message: err.Error()},
			})
			errorCount++
			continue
		}

		resp.Responses = append(resp.Responses, &repb.BatchUpdateBlobsResponse_Response{
			Digest: r.Digest,
			Status: &status.Status{Code: int32(codes.OK)},
		})
		successCount++
	}
	s.logger.Debug("BatchUpdateBlobs", "total", len(req.Requests), "success", successCount, "errors", errorCount)
	return resp, nil
}

func (s *ContentAddressableStorageServer) GetTree(req *repb.GetTreeRequest, stream repb.ContentAddressableStorage_GetTreeServer) error {
	return gstatus.Error(codes.Unimplemented, "GetTree not implemented")
}

type CapabilitiesServer struct {
	repb.UnimplementedCapabilitiesServer
	execEnabled bool
}

func NewCapabilitiesServer(execEnabled bool) *CapabilitiesServer {
	return &CapabilitiesServer{execEnabled: execEnabled}
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
			ExecEnabled:    s.execEnabled,
		},
		LowApiVersion:  &semver.SemVer{Major: 2, Minor: 0},
		HighApiVersion: &semver.SemVer{Major: 2, Minor: 2},
	}, nil
}
