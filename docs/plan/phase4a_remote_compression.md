# Phase 4a: Remote & Client Compression

## Objective
Implement transparent compression for traffic between:
1.  **Client <-> Proxy**: Allow clients (e.g., Bazel) to upload/download compressed blobs.
2.  **Proxy <-> Backing Cache (Tier 2)**: Reduce bandwidth usage to the remote backing cache.

## Protocol
We will adhere to the REAPI compression standard using `ByteStream` resource names:
*   **Uncompressed**: `[{instance_name}/]blobs/{hash}/{size}`
*   **Compressed**: `[{instance_name}/]compressed-blobs/zstd/{hash}/{original_size}`

The proxy will act as a transparent gateway, storing blobs **uncompressed** in Tier 1 (`LocalStore`).

### Flows
1.  **Client Upload (Write)**:
    *   If client uploads to `compressed-blobs/zstd`, Proxy decompresses on the fly -> `LocalStore`.
    *   If client uploads to `blobs`, Proxy stores directly -> `LocalStore`.
2.  **Client Download (Read)**:
    *   If client requests `compressed-blobs/zstd`, Proxy reads from `LocalStore` -> Compresses on the fly -> Client.
    *   If client requests `blobs`, Proxy reads from `LocalStore` -> Client.
3.  **Backing Cache Upload (Put)**:
    *   Proxy reads from `LocalStore`.
    *   If `BackingCacheConfig.Compression` is "zstd", Proxy compresses -> Remote.
    *   Else, Proxy sends uncompressed -> Remote.
4.  **Backing Cache Download (Get)**:
    *   If `BackingCacheConfig.Compression` is "zstd", Proxy requests `compressed-blobs/zstd` -> Decompresses -> `LocalStore`.
    *   Else, Proxy requests `blobs` -> `LocalStore`.

## Design

### 1. Configuration
Update `pkg/config/config.go` to add a `Compression` field to `BackingCacheConfig`:
```go
type BackingCacheConfig struct {
    Target      string `mapstructure:"target"`
    Compression string `mapstructure:"compression"` // "zstd" or empty/none
}
```

### 2. Capabilities
Update `pkg/server/server.go` (`CapabilitiesServer`) to advertise `ZSTD` support.

### 3. Server Implementation
Modify `pkg/server/bytestream.go`:
*   Update regex to match `compressed-blobs/zstd`.
*   **`Read`**: If resource name indicates compression:
    *   Get uncompressed reader from Store.
    *   Wrap with `zstd.NewWriter` (streaming compression).
*   **`Write`**: If resource name indicates compression:
    *   Wrap input stream with `zstd.NewReader` (streaming decompression).
    *   Write uncompressed data to Store.

### 4. RemoteStore Implementation (Completed)
`pkg/storage/remote.go` already handles the Proxy <-> Remote compression logic.

## Dependencies
*   `github.com/klauspost/compress/zstd`

## Testing
*   Verify `GetCapabilities` returns ZSTD.
*   Verify `ByteStream` handles compressed read/write.
*   Verify `RemoteStore` integration.