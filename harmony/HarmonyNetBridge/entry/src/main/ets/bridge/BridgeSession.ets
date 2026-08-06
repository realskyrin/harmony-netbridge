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
  StopRequestPayload
} from './HnbProtocol';

const BRIDGE_ADDRESS: string = '127.0.0.1';
const BRIDGE_PORT: number = 27183;

export enum BridgeConnectionState {
  IDLE = 'IDLE',
  CONNECTING = 'CONNECTING',
  CONNECTED = 'CONNECTED',
  DISCONNECTED = 'DISCONNECTED',
  ERROR = 'ERROR'
}

export class BridgeSnapshot {
  readonly state: BridgeConnectionState;
  readonly title: string;
  readonly detail: string;

  constructor(state: BridgeConnectionState, title: string, detail: string) {
    this.state = state;
    this.title = title;
    this.detail = detail;
  }
}

export type BridgeListener = (snapshot: BridgeSnapshot) => void;

// BridgeSession is the Phase 1 control connection. It intentionally does not
// create a VPN or share this live socket with VpnExtensionAbility.
export class BridgeSession {
  private client: socket.TCPSocket | undefined = undefined;
  private decoder: HnbFrameDecoder = new HnbFrameDecoder();
  private listener: BridgeListener | undefined = undefined;
  private current: BridgeSnapshot = new BridgeSnapshot(
    BridgeConnectionState.IDLE,
    '等待 Mac 服务',
    '先在 Mac 运行 harmony-netbridge start'
  );
  private manualClosing: boolean = false;
  private nextOutgoingSequence: number = 1;

  get snapshot(): BridgeSnapshot {
    return this.current;
  }

  setListener(listener: BridgeListener | undefined): void {
    this.listener = listener;
    if (listener !== undefined) {
      listener(this.current);
    }
  }

  async connect(): Promise<void> {
    if (this.client !== undefined || this.current.state === BridgeConnectionState.CONNECTING) {
      return;
    }
    this.manualClosing = false;
    this.decoder.reset();
    this.nextOutgoingSequence = 1;
    this.publish(new BridgeSnapshot(
      BridgeConnectionState.CONNECTING,
      '正在连接 Mac',
      `设备回环地址 ${BRIDGE_ADDRESS}:${BRIDGE_PORT}`
    ));

    const client: socket.TCPSocket = socket.constructTCPSocketInstance();
    this.client = client;
    client.on('message', (messageInfo: socket.SocketMessageInfo) => {
      this.onMessage(client, messageInfo.message);
    });
    client.on('close', () => {
      this.onClosed(client);
    });
    client.on('error', (error: BusinessError) => {
      this.onSocketError(client, error);
    });

    try {
      await client.connect({
        address: {
          address: BRIDGE_ADDRESS,
          port: BRIDGE_PORT
        },
        timeout: 5000
      });
      if (this.client !== client || this.manualClosing) {
        await this.closeClient(client);
        return;
      }
      await client.setExtraOptions({
        keepAlive: true,
        TCPNoDelay: true
      });
      if (this.client !== client || this.manualClosing) {
        await this.closeClient(client);
        return;
      }
      const hello: HelloPayload = {
        role: 'control',
        mode: 'phase1',
        appVersion: '0.1.0',
        supportedVersions: [1],
        capabilities: ['control'],
        message: 'hello'
      };
      await this.send(client, HnbFrameType.HELLO, encodeJsonPayload(hello));
      if (this.client !== client || this.manualClosing) {
        return;
      }
      this.publish(new BridgeSnapshot(
        BridgeConnectionState.CONNECTING,
        '已连接，等待 world',
        'HELLO 已通过 USB 控制通道发送'
      ));
    } catch (error) {
      if (this.client !== client) {
        return;
      }
      const businessError: BusinessError = error as BusinessError;
      this.publish(new BridgeSnapshot(
        BridgeConnectionState.ERROR,
        '无法连接 Mac',
        this.safeBusinessError(businessError)
      ));
      await this.closeClient(client);
    }
  }

  async disconnect(): Promise<void> {
    this.manualClosing = true;
    const client: socket.TCPSocket | undefined = this.client;
    if (client !== undefined) {
      await this.closeClient(client);
    }
    this.publish(new BridgeSnapshot(
      BridgeConnectionState.DISCONNECTED,
      '连接已关闭',
      '可以点击重新连接'
    ));
  }

  private async onMessage(client: socket.TCPSocket, chunk: ArrayBuffer): Promise<void> {
    if (this.client !== client) {
      return;
    }
    try {
      const frames: HnbFrame[] = this.decoder.push(chunk);
      for (let index: number = 0; index < frames.length; index++) {
        await this.handleFrame(client, frames[index]);
      }
    } catch (error) {
      const protocolError: Error = error as Error;
      this.publish(new BridgeSnapshot(
        BridgeConnectionState.ERROR,
        '协议错误',
        protocolError.message
      ));
      await this.closeClient(client);
    }
  }

  private async handleFrame(client: socket.TCPSocket, frame: HnbFrame): Promise<void> {
    switch (frame.type) {
      case HnbFrameType.HELLO_ACK: {
        const ack: HelloAckPayload = decodeJsonPayload<HelloAckPayload>(frame.payload);
        if (frame.sequence !== 1 || ack.selectedVersion !== 1 || ack.message !== 'world' ||
          !/^[0-9a-f]{32}$/.test(ack.sessionToken)) {
          throw new Error('Mac returned an invalid HELLO_ACK');
        }
        this.publish(new BridgeSnapshot(
          BridgeConnectionState.CONNECTED,
          'hello / world 成功',
          'Phase 1 USB 控制通道已连接'
        ));
        return;
      }
      case HnbFrameType.STOP_REQUEST: {
        const request: StopRequestPayload = decodeJsonPayload<StopRequestPayload>(frame.payload);
        await this.send(client, HnbFrameType.STOP_ACK, new Uint8Array(0));
        this.manualClosing = true;
        this.publish(new BridgeSnapshot(
          BridgeConnectionState.DISCONNECTED,
          'Mac 已停止服务',
          request.reason
        ));
        await this.closeClient(client);
        return;
      }
      case HnbFrameType.ERROR: {
        const errorPayload: ErrorPayload = decodeJsonPayload<ErrorPayload>(frame.payload);
        this.publish(new BridgeSnapshot(
          BridgeConnectionState.ERROR,
          `Mac 协议错误：${errorPayload.code}`,
          errorPayload.message
        ));
        await this.closeClient(client);
        return;
      }
      default:
        throw new Error(`Phase 1 received unexpected frame type ${frame.type}`);
    }
  }

  private async send(client: socket.TCPSocket, type: HnbFrameType, payload: Uint8Array): Promise<void> {
    const encoded: Uint8Array = encodeFrame(type, this.takeSequence(), payload);
    try {
      await client.send({ data: encoded.buffer as ArrayBuffer });
    } catch (error) {
      const businessError: BusinessError = error as BusinessError;
      throw new Error(`Socket send failed: ${businessError.code}`);
    }
  }

  private takeSequence(): number {
    const sequence: number = this.nextOutgoingSequence;
    this.nextOutgoingSequence = sequence === 0xFFFFFFFF ? 1 : sequence + 1;
    return sequence;
  }

  private async closeClient(client: socket.TCPSocket): Promise<void> {
    if (this.client === client) {
      this.client = undefined;
    }
    client.off('message');
    client.off('close');
    client.off('error');
    try {
      await client.close();
    } catch (error) {
      // Closing an already-closed socket is intentionally idempotent.
    }
  }

  private onClosed(client: socket.TCPSocket): void {
    if (this.client !== client) {
      return;
    }
    this.client = undefined;
    if (!this.manualClosing) {
      this.publish(new BridgeSnapshot(
        BridgeConnectionState.DISCONNECTED,
        'Mac 连接已断开',
        'Phase 1 不自动重连，请检查 USB 与 CLI 后手动重试'
      ));
    }
  }

  private onSocketError(client: socket.TCPSocket, error: BusinessError): void {
    if (this.client !== client || this.manualClosing) {
      return;
    }
    this.publish(new BridgeSnapshot(
      BridgeConnectionState.ERROR,
      'Socket 错误',
      this.safeBusinessError(error)
    ));
    this.closeClient(client);
  }

  private publish(snapshot: BridgeSnapshot): void {
    this.current = snapshot;
    if (this.listener !== undefined) {
      this.listener(snapshot);
    }
  }

  private safeBusinessError(error: BusinessError): string {
    return error.code === undefined ? '网络连接失败' : `错误 ${error.code}：${error.message}`;
  }
}

export const bridgeSession: BridgeSession = new BridgeSession();
