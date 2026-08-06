cmake_minimum_required(VERSION 3.5.0)
project(HarmonyNetBridge)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

add_library(hnb SHARED packet_probe.cpp packet_pump.cpp)
target_link_libraries(hnb PUBLIC libace_napi.z.so libhilog_ndk.z.so)
