# Phase 5: Simple Deployment

This plan outlines the steps to create a simple deployment for goREgo, including containerization and a basic Helm chart for Kubernetes.

## 1. OCI Containerization

We will use the `rules_oci` Bazel rules to build a container image for goREgo.

### 1.1. Container Base

-   **Base Image:** `ubuntu:noble`
-   **Architectures:** `arm64` and `amd64`

### 1.2. Container Contents

-   **goREgo Binary:** The compiled binary from `//cmd/gorego:gorego`.
-   **linux-sandbox:** The binary from `//src/main/tools:linux-sandbox`.
-   **Dependencies:** `zip` and `unzip` will be installed via `apt`.
-   **Configuration:** The default `config.yaml` will be included in the image.

### 1.3. Build Rules

-   Create a `container` directory at the root of the repository.
-   Create `container/BUILD.bazel` and `container/defs.bzl` to define the container build rules.
-   The build rules will produce an OCI image that can be loaded into Docker or other container runtimes.

## 2. Helm Chart

We will create a basic Helm chart to deploy goREgo to a Kubernetes cluster for local testing.

### 2.1. Chart Structure

-   Create a `charts/gorego` directory.
-   The chart will include templates for:
    -   `Deployment`: To run the goREgo container.
    -   `Service`: To expose the goREgo gRPC port.
    -   `ConfigMap`: To manage the `config.yaml` file.
    -   `NOTES.txt`: To provide instructions on how to use the chart.

### 2.2. Chart Values

The `values.yaml` file will allow customization of:
-   Image repository and tag.
-   Number of replicas.
-   Service type and port.
-   Resource requests and limits.
-   `config.yaml` settings.

## 3. Implementation Steps

1.  Add `rules_oci` to the `bazel_deps.bzl`.
2.  Create the `container/BUILD.bazel` and `container/defs.bzl` files.
3.  Implement the OCI container build rules.
4.  Create the Helm chart structure.
5.  Implement the Helm chart templates and `values.yaml`.
6.  Test building the container and deploying the Helm chart to a local Kubernetes cluster (e.g., minikube or kind).
