package server

import (
	"testing"

	"github.com/colinrgodsey/goREgo/pkg/storage"
)

func TestParseResourceName(t *testing.T) {
	tests := []struct {
		name         string
		resourceName string
		isWrite      bool
		wantDigest   storage.Digest
		wantComp     bool
		wantErr      bool
	}{
		// Read cases
		{
			name:         "Read Blobs",
			resourceName: "blobs/deadbeef/123",
			isWrite:      false,
			wantDigest:   storage.Digest{Hash: "deadbeef", Size: 123},
			wantComp:     false,
			wantErr:      false,
		},
		{
			name:         "Read Compressed",
			resourceName: "compressed-blobs/zstd/deadbeef/123",
			isWrite:      false,
			wantDigest:   storage.Digest{Hash: "deadbeef", Size: 123},
			wantComp:     true,
			wantErr:      false,
		},
		{
			name:         "Read with instance",
			resourceName: "instance/blobs/deadbeef/123",
			isWrite:      false,
			wantDigest:   storage.Digest{Hash: "deadbeef", Size: 123},
			wantComp:     false,
			wantErr:      false,
		},
		{
			name:         "Read compressed with instance",
			resourceName: "instance/compressed-blobs/zstd/deadbeef/123",
			isWrite:      false,
			wantDigest:   storage.Digest{Hash: "deadbeef", Size: 123},
			wantComp:     true,
			wantErr:      false,
		},
		{
			name:         "Read Invalid format",
			resourceName: "blobs/deadbeef",
			isWrite:      false,
			wantErr:      true,
		},
		{
			name:         "Read Invalid hash",
			resourceName: "blobs/xyz/123", // Non-hex
			isWrite:      false,
			wantErr:      true,
		},

		// Write cases
		{
			name:         "Write Blobs",
			resourceName: "uploads/uuid/blobs/deadbeef/123",
			isWrite:      true,
			wantDigest:   storage.Digest{Hash: "deadbeef", Size: 123},
			wantComp:     false,
			wantErr:      false,
		},
		{
			name:         "Write Compressed",
			resourceName: "uploads/uuid/compressed-blobs/zstd/deadbeef/123",
			isWrite:      true,
			wantDigest:   storage.Digest{Hash: "deadbeef", Size: 123},
			wantComp:     true,
			wantErr:      false,
		},
		{
			name:         "Write with instance",
			resourceName: "instance/uploads/uuid/blobs/deadbeef/123",
			isWrite:      true,
			wantDigest:   storage.Digest{Hash: "deadbeef", Size: 123},
			wantComp:     false,
			wantErr:      false,
		},
		{
			name:         "Write compressed with instance",
			resourceName: "instance/uploads/uuid/compressed-blobs/zstd/deadbeef/123",
			isWrite:      true,
			wantDigest:   storage.Digest{Hash: "deadbeef", Size: 123},
			wantComp:     true,
			wantErr:      false,
		},
		{
			name:         "Write Invalid format (no uuid)",
			resourceName: "uploads/blobs/deadbeef/123",
			isWrite:      true,
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDigest, gotComp, err := parseResourceName(tt.resourceName, tt.isWrite)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseResourceName() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if gotDigest != tt.wantDigest {
					t.Errorf("parseResourceName() gotDigest = %v, want %v", gotDigest, tt.wantDigest)
				}
				if gotComp != tt.wantComp {
					t.Errorf("parseResourceName() gotComp = %v, want %v", gotComp, tt.wantComp)
				}
			}
		})
	}
}
