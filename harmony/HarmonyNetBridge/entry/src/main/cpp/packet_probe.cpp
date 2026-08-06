// Copyright 2026 HarmonyNetBridge Contributors
// SPDX-License-Identifier: Apache-2.0

#include "napi/native_api.h"
#include "packet_pump.h"

#include <arpa/inet.h>
#include <cerrno>
#include <chrono>
#include <cstdint>
#include <poll.h>
#include <string>
#include <vector>
#include <unistd.h>

namespace {

constexpr int32_t kMaxPacketSize = 65535;
constexpr uint32_t kProbeDestination = 0xC6120001; // 198.18.0.1 in host byte order.

void SetInt32(napi_env env, napi_value object, const char *name, int32_t value)
{
    napi_value property = nullptr;
    napi_create_int32(env, value, &property);
    napi_set_named_property(env, object, name, property);
}

void SetString(napi_env env, napi_value object, const char *name, const std::string &value)
{
    napi_value property = nullptr;
    napi_create_string_utf8(env, value.c_str(), value.size(), &property);
    napi_set_named_property(env, object, name, property);
}

napi_value MakeResult(napi_env env, int32_t status, int32_t length = 0, int32_t version = 0,
                      int32_t protocol = 0, const std::string &destination = "")
{
    napi_value result = nullptr;
    napi_create_object(env, &result);
    SetInt32(env, result, "status", status);
    SetInt32(env, result, "length", length);
    SetInt32(env, result, "version", version);
    SetInt32(env, result, "protocol", protocol);
    SetString(env, result, "destination", destination);
    return result;
}

napi_value ProbeTun(napi_env env, napi_callback_info info)
{
    size_t argc = 2;
    napi_value args[2] = {nullptr, nullptr};
    napi_get_cb_info(env, info, &argc, args, nullptr, nullptr);
    if (argc != 2) {
        return MakeResult(env, -EINVAL);
    }

    int32_t tunFd = -1;
    int32_t timeoutMs = 0;
    if (napi_get_value_int32(env, args[0], &tunFd) != napi_ok ||
        napi_get_value_int32(env, args[1], &timeoutMs) != napi_ok || tunFd < 0 || timeoutMs < 1) {
        return MakeResult(env, -EINVAL);
    }

    const auto deadline = std::chrono::steady_clock::now() + std::chrono::milliseconds(timeoutMs);
    std::vector<uint8_t> packet(kMaxPacketSize);
    int32_t lastLength = 0;
    int32_t lastVersion = 0;
    int32_t lastProtocol = 0;
    std::string lastDestination;

    // Creating a VPN may enqueue unrelated system packets before the explicit
    // Gate V datagram. Keep reading within the bounded timeout and match the
    // probe instead of assuming the first TUN packet belongs to this test.
    while (std::chrono::steady_clock::now() < deadline) {
        const auto remaining = std::chrono::duration_cast<std::chrono::milliseconds>(
            deadline - std::chrono::steady_clock::now());
        int remainingMs = static_cast<int>(remaining.count());
        if (remainingMs < 1) {
            remainingMs = 1;
        }
        pollfd descriptor = {.fd = tunFd, .events = POLLIN, .revents = 0};
        int pollResult = poll(&descriptor, 1, remainingMs);
        if (pollResult == 0) {
            break;
        }
        if (pollResult < 0) {
            if (errno == EINTR) {
                continue;
            }
            return MakeResult(env, -errno, lastLength, lastVersion, lastProtocol, lastDestination);
        }
        if ((descriptor.revents & POLLIN) == 0) {
            return MakeResult(env, -EIO, lastLength, lastVersion, lastProtocol, lastDestination);
        }

        ssize_t length = read(tunFd, packet.data(), packet.size());
        if (length < 0) {
            if (errno == EINTR || errno == EAGAIN || errno == EWOULDBLOCK) {
                continue;
            }
            return MakeResult(env, -errno, lastLength, lastVersion, lastProtocol, lastDestination);
        }
        lastLength = static_cast<int32_t>(length);
        if (length < 20) {
            continue;
        }

        lastVersion = packet[0] >> 4;
        lastProtocol = packet[9];
        uint32_t destination = static_cast<uint32_t>(packet[16]) << 24 |
            static_cast<uint32_t>(packet[17]) << 16 |
            static_cast<uint32_t>(packet[18]) << 8 |
            static_cast<uint32_t>(packet[19]);
        in_addr destinationAddress = {.s_addr = htonl(destination)};
        char destinationText[INET_ADDRSTRLEN] = {0};
        inet_ntop(AF_INET, &destinationAddress, destinationText, sizeof(destinationText));
        lastDestination = destinationText;

        if (lastVersion == 4 && lastProtocol == IPPROTO_UDP && destination == kProbeDestination) {
            return MakeResult(env, 0, lastLength, lastVersion, lastProtocol, lastDestination);
        }
    }
    return MakeResult(env, -ETIMEDOUT, lastLength, lastVersion, lastProtocol, lastDestination);
}

EXTERN_C_START
static napi_value Init(napi_env env, napi_value exports)
{
    napi_property_descriptor descriptors[] = {
        {"probeTun", nullptr, ProbeTun, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"connectData", nullptr, hnb::ConnectData, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"startPacketPump", nullptr, hnb::StartPacketPump, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"stopPacketPump", nullptr, hnb::StopPacketPump, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"getPacketPumpStatus", nullptr, hnb::GetPacketPumpStatus, nullptr, nullptr, nullptr, napi_default, nullptr},
    };
    napi_define_properties(env, exports, sizeof(descriptors) / sizeof(descriptors[0]), descriptors);
    return exports;
}
EXTERN_C_END

static napi_module module = {
    .nm_version = 1,
    .nm_flags = 0,
    .nm_filename = nullptr,
    .nm_register_func = Init,
    .nm_modname = "hnb",
    .nm_priv = nullptr,
    .reserved = {nullptr},
};

} // namespace

extern "C" __attribute__((constructor)) void RegisterHarmonyNetBridgeModule()
{
    napi_module_register(&module);
}
