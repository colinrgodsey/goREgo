# GoBazelRE

GoBazelRE is a high-performance, homogeneous Remote Execution (RE) service for Bazel, implemented in Go.

## Architecture

It uses a "Cell" model where every node is identical ("Proxy-Worker"), acting as both a caching proxy and an execution worker.
- **Coordination:** Peer-to-peer mesh using `hashicorp/memberlist`.
- **Storage:** Two-tiered architecture.
    - Tier 1: Local NVMe/SSD ephemeral cache with hardlinking.
    - Tier 2: Authoritative backing cache (e.g., S3, specialized storage node).
- **Scheduling:** Decentralized load-aware routing based on pending task counts.

See `docs/research/Homogeneous Go Bazel Remote Execution Architecture (Final Design).md` for details.
