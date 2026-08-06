import { BusinessError } from '@kit.BasicServicesKit';
import { socket } from '@kit.NetworkKit';
import {
  decodeJsonPayload,
  encodeFrame,
  encodeJsonPayload,
  HelloAckPayload,
  HelloPayload,
  HnbFrame,
  HnbFrameDecoder,
  HnbFrameType
} from '../bridge/HnbProtocol';

const BRIDGE_ADDRESS: string = '127.0.0.1';
const BRIDGE_PORT: number = 27183;
const HANDSHAKE_TIMEOUT_MS: number = 5000;

// GateTunnel owns the socket whose fd is passed to VpnConnection.protect().
// It uses the Phase 1 handshake only for the Gate V capability probe.
export class GateTunnel {
  private client: socket.TCPSocket | undefined = undefined;

  async connect(): Promise<number> {
    if (this.client !== undefined) {
      throw new Error('Gate V tunnel is already connected');
    }
    const client: socket.TCPSocket = socket.constructTCPSocketInstance();
    this.client = client;
    try {
      await client.connect({
        address: {
          address: BRIDGE_ADDRESS,
          port: BRIDGE_PORT
        },
        timeout: HANDSHAKE_TIMEOUT_MS
      });
      await client.setExtraOptions({
        keepAlive: true,
        TCPNoDelay: true
      });

      const acknowledgement: Promise<void> = this.waitForAcknowledgement(client);
      const hello: HelloPayload = {
        role: 'control',
        mode: 'phase1',
        appVersion: '0.4.0-gatev',
        supportedVersions: [1],
        capabilities: ['control', 'gate-v'],
        message: 'hello'
      };
      const frame: Uint8Array = encodeFrame(HnbFrameType.HELLO, 1, encodeJsonPayload(hello));
      await client.send({ data: frame.buffer as ArrayBuffer });
      await acknowledgement;
      return client.getSocketFd();
    } catch (error) {
      await this.close();
      const businessError: BusinessError = error as BusinessError;
      const detail: string = businessError.code === undefined ? businessError.message : `${businessError.code}`;
      throw new Error(`Gate V tunnel setup failed: ${detail}`);
    }
  }

  async close(): Promise<void> {
    const client: socket.TCPSocket | undefined = this.client;
    this.client = undefined;
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

  private waitForAcknowledgement(client: socket.TCPSocket): Promise<void> {
    return new Promise<void>((resolve, reject) => {
      const decoder: HnbFrameDecoder = new HnbFrameDecoder();
      let settled: boolean = false;
      const timeout: number = setTimeout(() => {
        if (!settled) {
          settled = true;
          reject(new Error('Gate V HELLO_ACK timed out'));
        }
      }, HANDSHAKE_TIMEOUT_MS);

      client.on('message', (messageInfo: socket.SocketMessageInfo) => {
        if (settled) {
          return;
        }
        try {
          const frames: HnbFrame[] = decoder.push(messageInfo.message);
          if (frames.length === 0) {
            return;
          }
          const frame: HnbFrame = frames[0];
          if (frame.type !== HnbFrameType.HELLO_ACK || frame.sequence !== 1) {
            throw new Error('Gate V tunnel received an invalid HELLO_ACK frame');
          }
          const ack: HelloAckPayload = decodeJsonPayload<HelloAckPayload>(frame.payload);
          if (ack.selectedVersion !== 1 || ack.message !== 'world' || !/^[0-9a-f]{32}$/.test(ack.sessionToken)) {
            throw new Error('Gate V tunnel received an invalid HELLO_ACK payload');
          }
          settled = true;
          clearTimeout(timeout);
          client.off('message');
          client.off('error');
          client.off('close');
          resolve();
        } catch (error) {
          settled = true;
          clearTimeout(timeout);
          reject(error as Error);
        }
      });
      client.on('error', (error: BusinessError) => {
        if (!settled) {
          settled = true;
          clearTimeout(timeout);
          reject(new Error(`Gate V tunnel socket error ${error.code}`));
        }
      });
      client.on('close', () => {
        if (!settled) {
          settled = true;
          clearTimeout(timeout);
          reject(new Error('Gate V tunnel closed during handshake'));
        }
      });
    });
  }
}
