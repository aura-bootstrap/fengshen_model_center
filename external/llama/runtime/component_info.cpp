// This entry intentionally does not link llama, load DLL backends or spawn a server.
#include <cstdio>
#include <cstring>
#include "component_profile.h"

int main(int argc, char ** argv) {
    if (argc != 2 || std::strcmp(argv[1], "--component-info") != 0) {
        std::fputs("Only --component-info is supported. Server execution is supervisor-internal.\n", stderr);
        return 64;
    }
    std::puts("{\"schema\":1,\"component_id\":\"llama-runtime\",\"version\":\"" LLAMA_COMPONENT_VERSION
        "\",\"profile\":\"" LLAMA_COMPONENT_PROFILE "\",\"driver_requirements\":[\"" LLAMA_COMPONENT_DRIVER
        "\"],\"upstream_commit\":\"fd27b2034905f8dadd8bd6d2cc44595f4929d391\",\"binary_source\":\"source-build\","
        "\"platform\":\"windows-amd64-cpu\",\"server\":\"internal/llama-server.exe\","
        "\"capabilities\":[\"text\",\"vision-explicit-projector\"],\"protocols\":[\"llama.cpp.server.http/b12182\"],"
        "\"metadata_only\":true,\"inference\":\"not_tested\"}");
    return 0;
}
