load("//rules/typescript:index.bzl", "ts_library")

package(default_visibility = ["//enterprise:__subpackages__"])

ts_library(
    name = "login",
    srcs = ["login.tsx"],
    deps = [
        "//app/alert:alert_service",
        "//app/auth:auth_service",
        "//app/capabilities",
        "//app/components/button",
        "//app/components/input",
        "//app/router",
        "//app/service:rpc_service",
        "//proto:group_ts_proto",
        "@npm//@types/react",
        "@npm//react",
        "@npm//tslib",
    ],
)
