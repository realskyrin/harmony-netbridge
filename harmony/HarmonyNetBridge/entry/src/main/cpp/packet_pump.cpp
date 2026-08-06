// Copyright 2026 HarmonyNetBridge Contributors
// SPDX-License-Identifier: Apache-2.0

#include "packet_pump.h"

#include <arpa/inet.h>
#include <atomic>
#include <cerrno>
#include <chrono>
#include <cstdint>
#include <cstring>
#include <fcntl.h>
#include <mutex>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <poll.h>
#include <string>
#include <sys/socket.h>
#include <thread>
#include <unistd.h>
#include <vector>

namespace hnb {
namespace {

constexpr size_t kHeaderSize = 16;
constexpr size_t kMaxPacketSize = 65535;
constexpr uint8_t kVersion = 1;
constexpr uint8_t kTypeDataHello = 0x10;
constexpr uint8_t kTypeDataAck = 0x11;
constexpr uint8_t kTypeIPPacket = 0x20;
constexpr uint8_t kTypePing = 0x30;
constexpr uint8_t kTypePong = 0x31;
constexpr int kPollIntervalMs = 250;
constexpr int kHeartbeatTimeoutMs = 20000;

uint16_t ReadUint16(const uint8_t *bytes)
{
    return static_cast<uint16_t>(bytes[0]) << 8 | static_cast<uint16_t>(bytes[1]);
}

uint32_t ReadUint32(const uint8_t *bytes)
{
    return static_cast<uint32_t>(bytes[0]) << 24 | static_cast<uint32_t>(bytes[1]) << 16 |
        static_cast<uint32_t>(bytes[2]) << 8 | static_cast<uint32_t>(bytes[3]);
}

void WriteUint16(uint8_t *bytes, uint16_t value)
{
    bytes[0] = static_cast<uint8_t>(value >> 8);
    bytes[1] = static_cast<uint8_t>(value);
}

void WriteUint32(uint8_t *bytes, uint32_t value)
{
    bytes[0] = static_cast<uint8_t>(value >> 24);
    bytes[1] = static_cast<uint8_t>(value >> 16);
    bytes[2] = static_cast<uint8_t>(value >> 8);
    bytes[3] = static_cast<uint8_t>(value);
}

void MakeHeader(uint8_t *header, uint8_t type, uint32_t length, uint32_t sequence)
{
    header[0] = 'H';
    header[1] = 'N';
    header[2] = 'B';
    header[3] = '1';
    header[4] = kVersion;
    header[5] = type;
    WriteUint16(header + 6, 0);
    WriteUint32(header + 8, length);
    WriteUint32(header + 12, sequence);
}

bool ValidToken(const std::string &token)
{
    if (token.size() != 32) {
        return false;
    }
    for (char character : token) {
        if ((character < '0' || character > '9') && (character < 'a' || character > 'f')) {
            return false;
        }
    }
    return true;
}

int WriteAll(int fd, const uint8_t *data, size_t length)
{
    size_t offset = 0;
    while (offset < length) {
        ssize_t written = send(fd, data + offset, length - offset, MSG_NOSIGNAL);
        if (written < 0) {
            if (errno == EINTR) {
                continue;
            }
            return -errno;
        }
        if (written == 0) {
            return -EPIPE;
        }
        offset += static_cast<size_t>(written);
    }
    return 0;
}

int ReadAllBefore(int fd, uint8_t *data, size_t length, std::chrono::steady_clock::time_point deadline)
{
    size_t offset = 0;
    while (offset < length) {
        auto remaining = std::chrono::duration_cast<std::chrono::milliseconds>(deadline - std::chrono::steady_clock::now());
        if (remaining.count() <= 0) {
            return -ETIMEDOUT;
        }
        pollfd descriptor = {.fd = fd, .events = POLLIN, .revents = 0};
        int result = poll(&descriptor, 1, static_cast<int>(remaining.count()));
        if (result == 0) {
            return -ETIMEDOUT;
        }
        if (result < 0) {
            if (errno == EINTR) {
                continue;
            }
            return -errno;
        }
        if ((descriptor.revents & POLLIN) == 0) {
            return -ECONNRESET;
        }
        ssize_t readLength = recv(fd, data + offset, length - offset, 0);
        if (readLength < 0) {
            if (errno == EINTR || errno == EAGAIN || errno == EWOULDBLOCK) {
                continue;
            }
            return -errno;
        }
        if (readLength == 0) {
            return -ECONNRESET;
        }
        offset += static_cast<size_t>(readLength);
    }
    return 0;
}

uint32_t NextSequence(uint32_t sequence)
{
    return sequence == UINT32_MAX ? 1 : sequence + 1;
}

class PacketPump {
public:
    static PacketPump &Instance()
    {
        static PacketPump instance;
        return instance;
    }

    int Connect(const std::string &address, uint16_t port, const std::string &token, int timeoutMs)
    {
        std::lock_guard<std::mutex> lock(operationMutex_);
        StopLocked();
        if (!ValidToken(token) || timeoutMs < 1) {
            return -EINVAL;
        }

        int fd = socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC, IPPROTO_TCP);
        if (fd < 0) {
            return -errno;
        }
        sockaddr_in destination = {};
        destination.sin_family = AF_INET;
        destination.sin_port = htons(port);
        if (inet_pton(AF_INET, address.c_str(), &destination.sin_addr) != 1) {
            close(fd);
            return -EINVAL;
        }
        int flags = fcntl(fd, F_GETFL, 0);
        if (flags < 0 || fcntl(fd, F_SETFL, flags | O_NONBLOCK) < 0) {
            int result = -errno;
            close(fd);
            return result;
        }
        int connectResult = connect(fd, reinterpret_cast<sockaddr *>(&destination), sizeof(destination));
        if (connectResult < 0 && errno != EINPROGRESS) {
            int result = -errno;
            close(fd);
            return result;
        }
        if (connectResult < 0) {
            pollfd descriptor = {.fd = fd, .events = POLLOUT, .revents = 0};
            int pollResult;
            do {
                pollResult = poll(&descriptor, 1, timeoutMs);
            } while (pollResult < 0 && errno == EINTR);
            if (pollResult <= 0) {
                int result = pollResult == 0 ? -ETIMEDOUT : -errno;
                close(fd);
                return result;
            }
            int socketError = 0;
            socklen_t errorLength = sizeof(socketError);
            if (getsockopt(fd, SOL_SOCKET, SO_ERROR, &socketError, &errorLength) < 0 || socketError != 0) {
                int result = socketError == 0 ? -errno : -socketError;
                close(fd);
                return result;
            }
        }
        if (fcntl(fd, F_SETFL, flags) < 0) {
            int result = -errno;
            close(fd);
            return result;
        }
        int enabled = 1;
        setsockopt(fd, SOL_SOCKET, SO_KEEPALIVE, &enabled, sizeof(enabled));
        setsockopt(fd, IPPROTO_TCP, TCP_NODELAY, &enabled, sizeof(enabled));

        std::string payload = "{\"sessionToken\":\"" + token + "\",\"role\":\"data\"}";
        uint8_t header[kHeaderSize] = {};
        MakeHeader(header, kTypeDataHello, static_cast<uint32_t>(payload.size()), 1);
        int result = WriteAll(fd, header, sizeof(header));
        if (result == 0) {
            result = WriteAll(fd, reinterpret_cast<const uint8_t *>(payload.data()), payload.size());
        }
        auto deadline = std::chrono::steady_clock::now() + std::chrono::milliseconds(timeoutMs);
        uint8_t acknowledgement[kHeaderSize] = {};
        if (result == 0) {
            result = ReadAllBefore(fd, acknowledgement, sizeof(acknowledgement), deadline);
        }
        if (result == 0 && (std::memcmp(acknowledgement, "HNB1", 4) != 0 || acknowledgement[4] != kVersion ||
            acknowledgement[5] != kTypeDataAck || ReadUint16(acknowledgement + 6) != 0 ||
            ReadUint32(acknowledgement + 8) != 0 || ReadUint32(acknowledgement + 12) != 1)) {
            result = -EBADMSG;
        }
        if (result != 0) {
            close(fd);
            return result;
        }

        socketFd_ = fd;
        connected_.store(true);
        lastError_.store(0);
        return fd;
    }

    int Start(int tunFd)
    {
        std::lock_guard<std::mutex> lock(operationMutex_);
        if (tunFd < 0 || socketFd_ < 0 || !connected_.load() || tunToSocket_.joinable() || socketToTun_.joinable()) {
            return -EINVAL;
        }
        tunFd_ = tunFd;
        running_.store(true);
        lastError_.store(0);
        packetsFromTun_.store(0);
        bytesFromTun_.store(0);
        packetsToTun_.store(0);
        bytesToTun_.store(0);
        droppedPackets_.store(0);
        outgoingSequence_ = 2;
        int dataFd = socketFd_;
        tunToSocket_ = std::thread([this, tunFd, dataFd]() { PumpTunToSocket(tunFd, dataFd); });
        socketToTun_ = std::thread([this, tunFd, dataFd]() { PumpSocketToTun(tunFd, dataFd); });
        return 0;
    }

    void Stop()
    {
        std::lock_guard<std::mutex> lock(operationMutex_);
        StopLocked();
    }

    int DataFd()
    {
        std::lock_guard<std::mutex> lock(operationMutex_);
        return socketFd_;
    }

    bool Running() const { return running_.load(); }
    bool Connected() const { return connected_.load(); }
    int LastError() const { return lastError_.load(); }
    uint64_t PacketsFromTun() const { return packetsFromTun_.load(); }
    uint64_t BytesFromTun() const { return bytesFromTun_.load(); }
    uint64_t PacketsToTun() const { return packetsToTun_.load(); }
    uint64_t BytesToTun() const { return bytesToTun_.load(); }
    uint64_t DroppedPackets() const { return droppedPackets_.load(); }

private:
    PacketPump() = default;
    ~PacketPump() { Stop(); }
    PacketPump(const PacketPump &) = delete;
    PacketPump &operator=(const PacketPump &) = delete;

    void StopLocked()
    {
        running_.store(false);
        if (socketFd_ >= 0) {
            shutdown(socketFd_, SHUT_RDWR);
        }
        if (tunToSocket_.joinable()) {
            tunToSocket_.join();
        }
        if (socketToTun_.joinable()) {
            socketToTun_.join();
        }
        if (socketFd_ >= 0) {
            close(socketFd_);
        }
        socketFd_ = -1;
        tunFd_ = -1;
        connected_.store(false);
    }

    void Fail(int error, int dataFd)
    {
        int expected = 0;
        lastError_.compare_exchange_strong(expected, error == 0 ? -EIO : error);
        running_.store(false);
        shutdown(dataFd, SHUT_RDWR);
    }

    int SendFrame(int dataFd, uint8_t type, const uint8_t *payload, uint32_t length)
    {
        std::lock_guard<std::mutex> lock(writeMutex_);
        uint8_t header[kHeaderSize] = {};
        MakeHeader(header, type, length, outgoingSequence_);
        int result = WriteAll(dataFd, header, sizeof(header));
        if (result == 0 && length > 0) {
            result = WriteAll(dataFd, payload, length);
        }
        if (result == 0) {
            outgoingSequence_ = NextSequence(outgoingSequence_);
        }
        return result;
    }

    void PumpTunToSocket(int tunFd, int dataFd)
    {
        std::vector<uint8_t> packet(kMaxPacketSize);
        while (running_.load()) {
            pollfd descriptor = {.fd = tunFd, .events = POLLIN, .revents = 0};
            int pollResult = poll(&descriptor, 1, kPollIntervalMs);
            if (pollResult == 0) {
                continue;
            }
            if (pollResult < 0) {
                if (errno == EINTR) {
                    continue;
                }
                Fail(-errno, dataFd);
                return;
            }
            if ((descriptor.revents & POLLIN) == 0) {
                Fail(-EIO, dataFd);
                return;
            }
            ssize_t length = read(tunFd, packet.data(), packet.size());
            if (length < 0) {
                if (errno == EINTR || errno == EAGAIN || errno == EWOULDBLOCK) {
                    continue;
                }
                Fail(-errno, dataFd);
                return;
            }
            if (length < 20 || packet[0] >> 4 != 4) {
                droppedPackets_.fetch_add(1);
                continue;
            }
            uint16_t totalLength = ReadUint16(packet.data() + 2);
            if (totalLength < 20 || totalLength > static_cast<size_t>(length)) {
                droppedPackets_.fetch_add(1);
                continue;
            }
            int result = SendFrame(dataFd, kTypeIPPacket, packet.data(), totalLength);
            if (result != 0) {
                Fail(result, dataFd);
                return;
            }
            packetsFromTun_.fetch_add(1);
            bytesFromTun_.fetch_add(totalLength);
        }
    }

    void PumpSocketToTun(int tunFd, int dataFd)
    {
        std::vector<uint8_t> packet(kMaxPacketSize);
        uint32_t expectedSequence = 2;
        while (running_.load()) {
            uint8_t header[kHeaderSize] = {};
            auto headerDeadline = std::chrono::steady_clock::now() + std::chrono::milliseconds(kHeartbeatTimeoutMs);
            int result = ReadAllBefore(dataFd, header, sizeof(header), headerDeadline);
            if (result != 0) {
                if (running_.load()) {
                    Fail(result, dataFd);
                }
                return;
            }
            uint32_t payloadLength = ReadUint32(header + 8);
            uint8_t frameType = header[5];
            if (std::memcmp(header, "HNB1", 4) != 0 || header[4] != kVersion ||
                ReadUint16(header + 6) != 0 || payloadLength > kMaxPacketSize ||
                ReadUint32(header + 12) != expectedSequence) {
                Fail(-EBADMSG, dataFd);
                return;
            }
            if ((frameType == kTypeIPPacket && payloadLength < 20) ||
                (frameType == kTypePing && payloadLength != 8) ||
                (frameType != kTypeIPPacket && frameType != kTypePing)) {
                Fail(-EBADMSG, dataFd);
                return;
            }
            auto payloadDeadline = std::chrono::steady_clock::now() + std::chrono::milliseconds(kHeartbeatTimeoutMs);
            result = ReadAllBefore(dataFd, packet.data(), payloadLength, payloadDeadline);
            if (result != 0) {
                Fail(result, dataFd);
                return;
            }
            expectedSequence = NextSequence(expectedSequence);
            if (frameType == kTypePing) {
                result = SendFrame(dataFd, kTypePong, packet.data(), payloadLength);
                if (result != 0) {
                    Fail(result, dataFd);
                    return;
                }
                continue;
            }
            if (packet[0] >> 4 != 4 || ReadUint16(packet.data() + 2) != payloadLength) {
                Fail(-EBADMSG, dataFd);
                return;
            }
            ssize_t written;
            do {
                written = write(tunFd, packet.data(), payloadLength);
            } while (written < 0 && errno == EINTR);
            if (written != static_cast<ssize_t>(payloadLength)) {
                Fail(written < 0 ? -errno : -EIO, dataFd);
                return;
            }
            packetsToTun_.fetch_add(1);
            bytesToTun_.fetch_add(payloadLength);
        }
    }

    mutable std::mutex operationMutex_;
    std::mutex writeMutex_;
    int socketFd_ = -1;
    int tunFd_ = -1;
    std::thread tunToSocket_;
    std::thread socketToTun_;
    uint32_t outgoingSequence_ = 2;
    std::atomic<bool> connected_ = false;
    std::atomic<bool> running_ = false;
    std::atomic<int> lastError_ = 0;
    std::atomic<uint64_t> packetsFromTun_ = 0;
    std::atomic<uint64_t> bytesFromTun_ = 0;
    std::atomic<uint64_t> packetsToTun_ = 0;
    std::atomic<uint64_t> bytesToTun_ = 0;
    std::atomic<uint64_t> droppedPackets_ = 0;
};

void SetInt32(napi_env env, napi_value object, const char *name, int32_t value)
{
    napi_value property = nullptr;
    napi_create_int32(env, value, &property);
    napi_set_named_property(env, object, name, property);
}

void SetDouble(napi_env env, napi_value object, const char *name, double value)
{
    napi_value property = nullptr;
    napi_create_double(env, value, &property);
    napi_set_named_property(env, object, name, property);
}

void SetBoolean(napi_env env, napi_value object, const char *name, bool value)
{
    napi_value property = nullptr;
    napi_get_boolean(env, value, &property);
    napi_set_named_property(env, object, name, property);
}

bool GetString(napi_env env, napi_value value, std::string &output)
{
    size_t length = 0;
    if (napi_get_value_string_utf8(env, value, nullptr, 0, &length) != napi_ok || length > 1024) {
        return false;
    }
    std::vector<char> text(length + 1);
    size_t copied = 0;
    if (napi_get_value_string_utf8(env, value, text.data(), text.size(), &copied) != napi_ok) {
        return false;
    }
    output.assign(text.data(), copied);
    return true;
}

napi_value MakeConnectResult(napi_env env, int status, int fd)
{
    napi_value result = nullptr;
    napi_create_object(env, &result);
    SetInt32(env, result, "status", status);
    SetInt32(env, result, "fd", fd);
    return result;
}

} // namespace

napi_value ConnectData(napi_env env, napi_callback_info info)
{
    size_t argc = 4;
    napi_value args[4] = {nullptr, nullptr, nullptr, nullptr};
    napi_get_cb_info(env, info, &argc, args, nullptr, nullptr);
    int32_t port = 0;
    int32_t timeoutMs = 0;
    std::string address;
    std::string token;
    if (argc != 4 || !GetString(env, args[0], address) || napi_get_value_int32(env, args[1], &port) != napi_ok ||
        !GetString(env, args[2], token) || napi_get_value_int32(env, args[3], &timeoutMs) != napi_ok ||
        port < 1 || port > 65535) {
        return MakeConnectResult(env, -EINVAL, -1);
    }
    int result = PacketPump::Instance().Connect(address, static_cast<uint16_t>(port), token, timeoutMs);
    return MakeConnectResult(env, result < 0 ? result : 0, result < 0 ? -1 : result);
}

napi_value StartPacketPump(napi_env env, napi_callback_info info)
{
    size_t argc = 1;
    napi_value args[1] = {nullptr};
    napi_get_cb_info(env, info, &argc, args, nullptr, nullptr);
    int32_t tunFd = -1;
    int result = argc == 1 && napi_get_value_int32(env, args[0], &tunFd) == napi_ok ?
        PacketPump::Instance().Start(tunFd) : -EINVAL;
    napi_value value = nullptr;
    napi_create_int32(env, result, &value);
    return value;
}

napi_value StopPacketPump(napi_env env, napi_callback_info info)
{
    (void)info;
    PacketPump::Instance().Stop();
    napi_value result = nullptr;
    napi_get_undefined(env, &result);
    return result;
}

napi_value GetPacketPumpStatus(napi_env env, napi_callback_info info)
{
    (void)info;
    PacketPump &pump = PacketPump::Instance();
    napi_value result = nullptr;
    napi_create_object(env, &result);
    SetBoolean(env, result, "connected", pump.Connected());
    SetBoolean(env, result, "running", pump.Running());
    SetInt32(env, result, "errorCode", pump.LastError());
    SetInt32(env, result, "dataFd", pump.DataFd());
    SetDouble(env, result, "packetsFromTun", static_cast<double>(pump.PacketsFromTun()));
    SetDouble(env, result, "bytesFromTun", static_cast<double>(pump.BytesFromTun()));
    SetDouble(env, result, "packetsToTun", static_cast<double>(pump.PacketsToTun()));
    SetDouble(env, result, "bytesToTun", static_cast<double>(pump.BytesToTun()));
    SetDouble(env, result, "droppedPackets", static_cast<double>(pump.DroppedPackets()));
    return result;
}

} // namespace hnb
