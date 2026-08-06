import { BusinessError } from '@kit.BasicServicesKit';
import { http } from '@kit.NetworkKit';
import { hilog } from '@kit.PerformanceAnalysisKit';

const DOMAIN: number = 0x484E;
const TAG: string = 'HNBProxyDiag';
const MITM_INSTALL_URL: string = 'http://mitm.it/';
const TIMEOUT_MS: number = 5000;

export function isMitmInstallPage(responseCode: number, body: string): boolean {
  if (responseCode !== 200 || body.length === 0) {
    return false;
  }
  const normalized: string = body.toLowerCase();
  return normalized.indexOf('mitmproxy') >= 0 || normalized.indexOf('certificate') >= 0;
}

// This HTTP-only probe is intentionally safe before CA installation. A pass
// proves that a port-80 request traversed TUN, USB, the Mac CONNECT adapter,
// and mitmweb. HTTPS decryption remains a separate, explicit trust action.
export class ProxyDiagnostics {
  async run(): Promise<string> {
    const request: http.HttpRequest = http.createHttp();
    try {
      const response: http.HttpResponse = await request.request(MITM_INSTALL_URL, {
        method: http.RequestMethod.GET,
        expectDataType: http.HttpDataType.STRING,
        usingCache: false,
        connectTimeout: TIMEOUT_MS,
        readTimeout: TIMEOUT_MS,
        addressFamily: http.AddressFamily.ONLY_V4
      });
      const body: string = response.result as string;
      const responseCode: number = response.responseCode as number;
      if (!isMitmInstallPage(responseCode, body)) {
        throw new Error(`mitm.it 返回非预期响应：${responseCode}`);
      }
      hilog.info(DOMAIN, TAG, 'PHASE4_PROXY_HTTP_PASS');
      return 'HTTP 抓包链路已验证；HTTPS 解密仍需手动安装 mitmproxy CA';
    } catch (error) {
      const businessError: BusinessError = error as BusinessError;
      const detail: string = businessError.code === undefined ? businessError.message : businessError.code.toString();
      throw new Error(`HTTP 抓包自检失败：${detail}`);
    } finally {
      request.destroy();
    }
  }
}
