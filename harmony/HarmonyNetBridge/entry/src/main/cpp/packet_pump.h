// Copyright 2026 HarmonyNetBridge Contributors
// SPDX-License-Identifier: Apache-2.0

#ifndef HARMONY_NETBRIDGE_PACKET_PUMP_H
#define HARMONY_NETBRIDGE_PACKET_PUMP_H

#include "napi/native_api.h"

namespace hnb {

napi_value ConnectData(napi_env env, napi_callback_info info);
napi_value StartPacketPump(napi_env env, napi_callback_info info);
napi_value StopPacketPump(napi_env env, napi_callback_info info);
napi_value GetPacketPumpStatus(napi_env env, napi_callback_info info);

} // namespace hnb

#endif // HARMONY_NETBRIDGE_PACKET_PUMP_H
