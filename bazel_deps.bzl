load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")

def _bazel_source_impl(ctx):
    http_archive(
        name = "bazel",
        urls = ["https://github.com/bazelbuild/bazel/releases/download/8.5.1/bazel-8.5.1-dist.zip"],
        sha256 = "bf66a1cbaaafec32e1e103d0e07343082f1f0f3f20ad4c6b66c4eda3f690ed4d",
        patch_cmds = [
            # Create dummy execution_statistics.pb.h in tools dir
            """cat > src/main/tools/execution_statistics.pb.h <<EOF
#ifndef DUMMY_STATS_H
#define DUMMY_STATS_H
#include <string>
namespace tools {
namespace protos {
class ResourceUsage {
public:
  void set_utime_sec(long) {}
  void set_utime_usec(long) {}
  void set_stime_sec(long) {}
  void set_stime_usec(long) {}
  void set_maxrss(long) {}
  void set_ixrss(long) {}
  void set_idrss(long) {}
  void set_isrss(long) {}
  void set_minflt(long) {}
  void set_majflt(long) {}
  void set_nswap(long) {}
  void set_inblock(long) {}
  void set_oublock(long) {}
  void set_msgsnd(long) {}
  void set_msgrcv(long) {}
  void set_nsignals(long) {}
  void set_nvcsw(long) {}
  void set_nivcsw(long) {}
};
class ExecutionStatistics {
public:
  ResourceUsage* mutable_resource_usage() { return &ru; }
  std::string SerializeAsString() { return ""; }
  ResourceUsage ru;
};
}
}
#endif
EOF""",
            # Patch process-tools.cc to include local header
            "sed -i 's|src/main/protobuf/execution_statistics.pb.h|execution_statistics.pb.h|' src/main/tools/process-tools.cc",
            "rm src/main/tools/BUILD",
            """cat > src/main/tools/BUILD <<EOF
cc_binary(
    name = "linux-sandbox",
    srcs = [
        "execution_statistics.pb.h",
        "linux-sandbox.cc",
        "linux-sandbox.h",
        "linux-sandbox-pid1.cc",
        "linux-sandbox-pid1.h",
        "linux-sandbox-options.cc",
        "linux-sandbox-options.h",
        "logging.cc",
        "logging.h",
        "process-tools.cc",
        "process-tools.h",
        "process-tools-linux.cc",
    ],
    visibility = ["//visibility:public"],
)
EOF""",
        ],
    )

bazel_source = module_extension(
    implementation = _bazel_source_impl,
)
