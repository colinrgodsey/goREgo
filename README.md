# Go, RE, Go!

GoREGo is a high-performance, homogeneous Remote Execution (RE) service for Bazel, implemented in Go.

## Architecture

It uses a "Cell" model where every node is identical ("Proxy-Worker"), acting as both a caching proxy and an execution worker.
- **Coordination:** Peer-to-peer mesh using `hashicorp/memberlist`.
- **Storage:** Two-tiered architecture.
    - Tier 1: Local fast ephemeral cache with hardlinking.
    - Tier 2: Authoritative backing cache (e.g., S3, [bazel-remote](https://github.com/buchgr/bazel-remote), specialized storage node).
- **Scheduling:** Decentralized load-aware routing based on pending task counts.
- **Isolation:** **BYO-Environment.** GoREGo runs commands *au naturel*. It provides no built-in sandboxing. If you want a clean room, wrap the GoREGo binary in a container, VM, or chroot. We just execute the bytes.

See `docs/research/Homogeneous Go Bazel Remote Execution Architecture (Final Design).md` for details.
