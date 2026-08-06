import { BusinessError } from '@kit.BasicServicesKit';
import { socket } from '@kit.NetworkKit';
import {
  decodeJsonPayload,
  encodeFrame,
  encodeJsonPayload,
  ErrorPayload,
  HelloAckPayload,
  HelloPayload,
  HnbFrame,
  HnbFrameDecoder,
  HnbFrameType,
  isSupportedMtu,
  StopRequestPayload,
  VpnStatusPayload
} from '../bridge/HnbProtocol';

const BRIDGE_ADDRESS: string = '127.0.0.1';
const BRIDGE_PORT: number = 27183;
const HANDSHAKE_TIMEOUT_MS: number = 5000;
const HEARTBEAT_CHECK_INTERVAL_MS: number = 5000;
const HEARTBEAT_TIMEOUT_MS: number = 20000;

export class VpnControlHandshake {
  readonly socketFd: number;
  readonly sessionToken: string;
  readonly mtu: number;

  constructor(socketFd: number, sessionToken: string, mtu: number) {
    this.socketFd = socketFd;
    this.sessionToken = sessionToken;
    this.mtu = mtu;
  }
}

type HandshakeResolve = (handshake: VpnControlHandshake) => void;
type HandshakeReject = (error: Error) => void;
type StopResolve = (reason: string) => void;

// VpnControlSession remains in ArkTS so STOP and VPN_STATUS never contend with
// the high-volume native data socket.
export class VpnControlSession {
  private client: socket.TCPSocket | undefined = undefined;
  private decoder: HnbFrameDecoder = new HnbFrameDecoder();
  private nextOutgoingSequence: number = 1;
  private expectedIncomingSequence: number = 1;
  private handshakeResolve: HandshakeResolve | undefined = undefined;
  private handshakeReject: HandshakeReject | undefined = undefined;
  private stopResolve: StopResolve | undefined = undefined;
  private handshakeComplete: boolean = false;
  private closing: boolean = false;
  private handshakeTimer: number = -1;
  private heartbeatTimer: number = -1;
  private lastMacFrameAt: number = 0;
  private messageQueue: Promise<void> = Promise.resolve();
  private sendQueue: Promise<void> = Promise.resolve();
  private readonly handshakePromise: Promise<VpnControlHandshake>;
  private readonly stopPromise: Promise<string>;

  constructor() {
    this.handshakePromise = new Promise<VpnControlHandshake>((resolve: HandshakeResolve, reject: HandshakeReject) => {
      this.handshakeResolve = resolve;
      this.handshakeReject = reject;
    });
    this.stopPromise = new Promise<string>((resolve: StopResolve) => {
      this.stopResolve = resolve;
    });
  }

  async connect(): Promise<VpnControlHandshake> {
    if (this.client !== undefined) {
      throw new Error('VPN control session is already connected');
    }
    const client: socket.TCPSocket = socket.constructTCPSocketInstance();
    this.client = client;
    client.on('message', (messageInfo: socket.SocketMessageInfo) => {
      const chunk: ArrayBuffer = messageInfo.message.slice(0);
      this.messageQueue = this.messageQueue.then(async (): Promise<void> => {
        await this.onMessage(client, chunk);
      });
    });
    client.on('error', (error: BusinessError) => {
      this.fail(client, new Error(`VPN control socket error ${error.code}`));
    });
    client.on('close', () => {
      this.fail(client, new Error('VPN control socket closed'));
    });

    try {
      await client.connect({
        address: { address: BRIDGE_ADDRESS, port: BRIDGE_PORT },
        timeout: HANDSHAKE_TIMEOUT_MS
      });
      await client.setExtraOptions({ keepAlive: true, TCPNoDelay: true });
      const hello: HelloPayload = {
        role: 'control',
        mode: 'vpn',
        appVersion: '0.3.0',
        supportedVersions: [1],
        capabilities: ['control', 'data', 'tcp', 'udp', 'dns', 'heartbeat', 'reconnect', 'mtu'],
        message: 'hello'
      };
      this.handshakeTimer = setTimeout(() => {
        this.fail(client, new Error('VPN control HELLO_ACK timed out'));
      }, HANDSHAKE_TIMEOUT_MS);
      await this.send(client, HnbFrameType.HELLO, encodeJsonPayload(hello));
      return await this.handshakePromise;
    } catch (error) {
      const businessError: BusinessError = error as BusinessError;
      await this.close();
      throw new Error(`VPN control setup failed: ${businessError.code === undefined ? businessError.message : businessError.code}`);
    }
  }

  waitForStop(): Promise<string> {
    return this.stopPromise;
  }

  async reportStatus(state: string, message: string): Promise<void> {
    const client: socket.TCPSocket | undefined = this.client;
    if (client === undefined || !this.handshakeComplete) {
      return;
    }
    const payload: VpnStatusPayload = { state: state, message: message };
    await this.send(client, HnbFrameType.VPN_STATUS, encodeJsonPayload(payload));
  }

  async close(): Promise<void> {
    this.closing = true;
    this.clearHandshakeTimer();
    this.clearHeartbeatTimer();
    const client: socket.TCPSocket | undefined = this.client;
    this.client = undefined;
    this.resolveStop('local_stop');
    if (client === undefined) {
      return;
    }
    client.off('message');
    client.off('error');
    client.off('close');
    try {
      await client.close();
    } catch (error) {
      // Closing is intentionally idempotent.
    }
  }

  private async onMessage(client: socket.TCPSocket, chunk: ArrayBuffer): Promise<void> {
    if (this.client !== client || this.closing) {
      return;
    }
    try {
      const frames: HnbFrame[] = this.decoder.push(chunk);
      for (let index: number = 0; index < frames.length; index++) {
        await this.handleFrame(client, frames[index]);
      }
    } catch (error) {
      this.fail(client, error as Error);
    }
  }

  private async handleFrame(client: socket.TCPSocket, frame: HnbFrame): Promise<void> {
    if (frame.sequence !== this.expectedIncomingSequence) {
      throw new Error('VPN control frame sequence is not contiguous');
    }
    this.expectedIncomingSequence = this.nextSequence(this.expectedIncomingSequence);
    this.lastMacFrameAt = Date.now();
    if (!this.handshakeComplete) {
      if (frame.type !== HnbFrameType.HELLO_ACK || frame.sequence !== 1) {
        throw new Error('VPN control received an invalid HELLO_ACK');
      }
      const acknowledgement: HelloAckPayload = decodeJsonPayload<HelloAckPayload>(frame.payload);
      if (acknowledgement.selectedVersion !== 1 || acknowledgement.message !== 'world' ||
        !/^[0-9a-f]{32}$/.test(acknowledgement.sessionToken) ||
        acknowledgement.capabilities.indexOf('data') < 0 ||
        acknowledgement.capabilities.indexOf('heartbeat') < 0 ||
        acknowledgement.capabilities.indexOf('reconnect') < 0 ||
        !isSupportedMtu(acknowledgement.mtu)) {
        throw new Error('VPN control HELLO_ACK payload is invalid');
      }
      this.handshakeComplete = true;
      this.clearHandshakeTimer();
      this.startHeartbeatMonitor(client);
      const resolve: HandshakeResolve | undefined = this.handshakeResolve;
      this.handshakeResolve = undefined;
      this.handshakeReject = undefined;
      if (resolve !== undefined) {
        const socketFd: number = await client.getSocketFd();
        resolve(new VpnControlHandshake(socketFd, acknowledgement.sessionToken, acknowledgement.mtu));
      }
      return;
    }

    if (frame.type === HnbFrameType.STOP_REQUEST) {
      const request: StopRequestPayload = decodeJsonPayload<StopRequestPayload>(frame.payload);
      await this.send(client, HnbFrameType.STOP_ACK, new Uint8Array(0));
      this.resolveStop(request.reason);
      return;
    }
    if (frame.type === HnbFrameType.PING) {
      if (frame.payload.length !== 8) {
        throw new Error('VPN control PING payload is invalid');
      }
      await this.send(client, HnbFrameType.PONG, frame.payload);
      return;
    }
    if (frame.type === HnbFrameType.ERROR) {
      const errorPayload: ErrorPayload = decodeJsonPayload<ErrorPayload>(frame.payload);
      this.fail(client, new Error(`Mac protocol error ${errorPayload.code}`));
      return;
    }
    this.fail(client, new Error(`Unexpected VPN control frame ${frame.type}`));
  }

  private async send(client: socket.TCPSocket, type: HnbFrameType, payload: Uint8Array): Promise<void> {
    const frame: Uint8Array = encodeFrame(type, this.takeSequence(), payload);
    const operation: Promise<void> = this.sendQueue.then(async (): Promise<void> => {
      try {
        await client.send({ data: frame.buffer as ArrayBuffer });
      } catch (error) {
        const businessError: BusinessError = error as BusinessError;
        throw new Error(`VPN control send failed: ${businessError.code}`);
      }
    });
    this.sendQueue = operation.catch((): void => {
      // Keep the serialization chain usable so the caller receives the precise
      // failure from its own operation instead of a stale earlier rejection.
    });
    return operation;
  }

  private fail(client: socket.TCPSocket, error: Error): void {
    if (this.client !== client || this.closing) {
      return;
    }
    this.clearHandshakeTimer();
    this.clearHeartbeatTimer();
    if (!this.handshakeComplete) {
      const reject: HandshakeReject | undefined = this.handshakeReject;
      this.handshakeResolve = undefined;
      this.handshakeReject = undefined;
      if (reject !== undefined) {
        reject(error);
      }
    }
    this.resolveStop(`transport_lost:${error.message}`);
  }

  private resolveStop(reason: string): void {
    const resolve: StopResolve | undefined = this.stopResolve;
    this.stopResolve = undefined;
    if (resolve !== undefined) {
      resolve(reason);
    }
  }

  private clearHandshakeTimer(): void {
    if (this.handshakeTimer >= 0) {
      clearTimeout(this.handshakeTimer);
      this.handshakeTimer = -1;
    }
  }

  private startHeartbeatMonitor(client: socket.TCPSocket): void {
    this.lastMacFrameAt = Date.now();
    this.clearHeartbeatTimer();
    this.heartbeatTimer = setInterval(() => {
      if (Date.now() - this.lastMacFrameAt >= HEARTBEAT_TIMEOUT_MS) {
        this.fail(client, new Error('VPN control heartbeat timed out'));
      }
    }, HEARTBEAT_CHECK_INTERVAL_MS);
  }

  private clearHeartbeatTimer(): void {
    if (this.heartbeatTimer >= 0) {
      clearInterval(this.heartbeatTimer);
      this.heartbeatTimer = -1;
    }
  }

  private takeSequence(): number {
    const sequence: number = this.nextOutgoingSequence;
    this.nextOutgoingSequence = sequence === 0xFFFFFFFF ? 1 : sequence + 1;
    return sequence;
  }

  private nextSequence(sequence: number): number {
    return sequence === 0xFFFFFFFF ? 1 : sequence + 1;
  }
}
