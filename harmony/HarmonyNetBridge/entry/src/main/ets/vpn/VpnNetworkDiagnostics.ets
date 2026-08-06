import { BusinessError } from '@kit.BasicServicesKit';
import { socket } from '@kit.NetworkKit';
import { hilog } from '@kit.PerformanceAnalysisKit';
import {
  createDiagnosticDnsQuery,
  DnsTcpResponseDecoder,
  frameDnsOverTcp,
  validateDiagnosticDnsResponse
} from './DnsMessage';

const DOMAIN: number = 0x484E;
const TAG: string = 'HNBDiag';
const VIRTUAL_DNS: string = '198.18.0.1';
const DNS_PORT: number = 53;
const TIMEOUT_MS: number = 5000;

export class VpnDiagnosticResult {
  readonly udpAnswers: number;
  readonly tcpAnswers: number;

  constructor(udpAnswers: number, tcpAnswers: number) {
    this.udpAnswers = udpAnswers;
    this.tcpAnswers = tcpAnswers;
  }

  get detail(): string {
    return `UDP DNS ${this.udpAnswers} 条回答；TCP DNS ${this.tcpAnswers} 条回答`;
  }
}

// Both probes deliberately target the virtual DNS address installed in the
// VPN configuration. Success proves that application traffic entered TUN,
// crossed the native HNB data channel, traversed the Mac relay, and returned.
export class VpnNetworkDiagnostics {
  async run(): Promise<VpnDiagnosticResult> {
    const udpAnswers: number = await this.runUdpDns();
    hilog.info(DOMAIN, TAG, 'PHASE2_DIAG_UDP_DNS_PASS answers=%{public}d', udpAnswers);
    const tcpAnswers: number = await this.runTcpDns();
    hilog.info(DOMAIN, TAG, 'PHASE2_DIAG_TCP_DNS_PASS answers=%{public}d', tcpAnswers);
    hilog.info(DOMAIN, TAG, 'PHASE2_DIAG_PASS');
    return new VpnDiagnosticResult(udpAnswers, tcpAnswers);
  }

  private async runUdpDns(): Promise<number> {
    const client: socket.UDPSocket = socket.constructUDPSocketInstance();
    try {
      await client.bind({ address: '0.0.0.0', port: 0, family: 1 });
      const response: Promise<number> = this.waitForUdpResponse(client);
      const query: Uint8Array = createDiagnosticDnsQuery();
      await client.send({
        data: query.buffer as ArrayBuffer,
        address: { address: VIRTUAL_DNS, port: DNS_PORT, family: 1 }
      });
      return await response;
    } catch (error) {
      const businessError: BusinessError = error as BusinessError;
      throw new Error(`UDP DNS diagnostic failed: ${this.errorDetail(businessError)}`);
    } finally {
      client.off('message');
      client.off('error');
      try {
        await client.close();
      } catch (error) {
        // The probe owns the socket; closing is intentionally idempotent.
      }
    }
  }

  private waitForUdpResponse(client: socket.UDPSocket): Promise<number> {
    return new Promise<number>((resolve, reject) => {
      let settled: boolean = false;
      const timer: number = setTimeout(() => {
        if (!settled) {
          settled = true;
          reject(new Error('UDP DNS response timed out'));
        }
      }, TIMEOUT_MS);
      client.on('message', (messageInfo: socket.SocketMessageInfo) => {
        if (settled) {
          return;
        }
        try {
          const answerCount: number = validateDiagnosticDnsResponse(new Uint8Array(messageInfo.message));
          settled = true;
          clearTimeout(timer);
          resolve(answerCount);
        } catch (error) {
          settled = true;
          clearTimeout(timer);
          reject(error as Error);
        }
      });
      client.on('error', (error: BusinessError) => {
        if (!settled) {
          settled = true;
          clearTimeout(timer);
          reject(new Error(`UDP socket error ${error.code}`));
        }
      });
    });
  }

  private async runTcpDns(): Promise<number> {
    const client: socket.TCPSocket = socket.constructTCPSocketInstance();
    try {
      await client.connect({
        address: { address: VIRTUAL_DNS, port: DNS_PORT, family: 1 },
        timeout: TIMEOUT_MS
      });
      await client.setExtraOptions({ TCPNoDelay: true });
      const response: Promise<number> = this.waitForTcpResponse(client);
      const query: Uint8Array = frameDnsOverTcp(createDiagnosticDnsQuery());
      await client.send({ data: query.buffer as ArrayBuffer });
      return await response;
    } catch (error) {
      const businessError: BusinessError = error as BusinessError;
      throw new Error(`TCP DNS diagnostic failed: ${this.errorDetail(businessError)}`);
    } finally {
      client.off('message');
      client.off('error');
      try {
        await client.close();
      } catch (error) {
        // The probe owns the socket; closing is intentionally idempotent.
      }
    }
  }

  private waitForTcpResponse(client: socket.TCPSocket): Promise<number> {
    return new Promise<number>((resolve, reject) => {
      const decoder: DnsTcpResponseDecoder = new DnsTcpResponseDecoder();
      let settled: boolean = false;
      const timer: number = setTimeout(() => {
        if (!settled) {
          settled = true;
          reject(new Error('TCP DNS response timed out'));
        }
      }, TIMEOUT_MS);
      client.on('message', (messageInfo: socket.SocketMessageInfo) => {
        if (settled) {
          return;
        }
        try {
          const message: Uint8Array | undefined = decoder.push(messageInfo.message);
          if (message === undefined) {
            return;
          }
          const answerCount: number = validateDiagnosticDnsResponse(message);
          settled = true;
          clearTimeout(timer);
          resolve(answerCount);
        } catch (error) {
          settled = true;
          clearTimeout(timer);
          reject(error as Error);
        }
      });
      client.on('error', (error: BusinessError) => {
        if (!settled) {
          settled = true;
          clearTimeout(timer);
          reject(new Error(`TCP socket error ${error.code}`));
        }
      });
    });
  }

  private errorDetail(error: BusinessError): string {
    if (error.code !== undefined) {
      return error.code.toString();
    }
    return error.message;
  }
}
