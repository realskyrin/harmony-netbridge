import { BusinessError } from '@kit.BasicServicesKit';
import { http } from '@kit.NetworkKit';
import { hilog } from '@kit.PerformanceAnalysisKit';

const DOMAIN: number = 0x484E;
const TAG: string = 'HNBProxyDiag';
const TIMEOUT_MS: number = 5000;

export interface ProxyDiagnosticTarget {
  name: string;
  url: string;
}

const GOOGLE_TARGET: ProxyDiagnosticTarget = {
  name: 'Google',
  url: 'http://www.google.com/generate_204'
};
const BAIDU_TARGET: ProxyDiagnosticTarget = {
  name: '百度',
  url: 'http://www.baidu.com/'
};

export function selectProxyDiagnosticTarget(randomValue: number): ProxyDiagnosticTarget {
  return randomValue < 0.5 ? GOOGLE_TARGET : BAIDU_TARGET;
}

export function isSuccessfulProxyProbeResponse(responseCode: number): boolean {
  return responseCode >= 200 && responseCode < 400;
}

// This HTTP-only probe is intentionally safe before CA installation. A pass
// proves that a port-80 request traversed TUN, USB, the Mac CONNECT adapter,
// and mitmweb. HTTPS decryption remains a separate, explicit trust action.
export class ProxyDiagnostics {
  async run(): Promise<string> {
    const target: ProxyDiagnosticTarget = selectProxyDiagnosticTarget(Math.random());
    const request: http.HttpRequest = http.createHttp();
    try {
      const response: http.HttpResponse = await request.request(target.url, {
        method: http.RequestMethod.GET,
        expectDataType: http.HttpDataType.STRING,
        usingCache: false,
        connectTimeout: TIMEOUT_MS,
        readTimeout: TIMEOUT_MS,
        addressFamily: http.AddressFamily.ONLY_V4
      });
      const responseCode: number = response.responseCode as number;
      if (!isSuccessfulProxyProbeResponse(responseCode)) {
        throw new Error(`${target.name} 返回非预期响应：${responseCode}`);
      }
      hilog.info(DOMAIN, TAG, 'PHASE4_PROXY_HTTP_PASS');
      return `HTTP 抓包链路已通过 ${target.name} 验证；HTTPS 解密仍需手动安装 mitmproxy CA`;
    } catch (error) {
      const businessError: BusinessError = error as BusinessError;
      const detail: string = businessError.code === undefined ? businessError.message : businessError.code.toString();
      throw new Error(`HTTP 抓包自检失败：${detail}`);
    } finally {
      request.destroy();
    }
  }
}
