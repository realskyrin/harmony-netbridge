import { describe, expect, it } from '@ohos/hypium';
import {
  isSuccessfulProxyProbeResponse,
  selectProxyDiagnosticTarget
} from '../main/ets/vpn/ProxyDiagnostics';

export default function proxyDiagnosticsTest(): void {
  describe('ProxyDiagnostics', () => {
    it('selectsGoogleOrBaiduFromRandomValue', 0, () => {
      expect(selectProxyDiagnosticTarget(0).name).assertEqual('Google');
      expect(selectProxyDiagnosticTarget(0.49).url).assertEqual('http://www.google.com/generate_204');
      expect(selectProxyDiagnosticTarget(0.5).name).assertEqual('百度');
      expect(selectProxyDiagnosticTarget(0.99).url).assertEqual('http://www.baidu.com/');
    });

    it('acceptsSuccessfulHttpResponse', 0, () => {
      expect(isSuccessfulProxyProbeResponse(200)).assertTrue();
      expect(isSuccessfulProxyProbeResponse(204)).assertTrue();
      expect(isSuccessfulProxyProbeResponse(302)).assertTrue();
      expect(isSuccessfulProxyProbeResponse(199)).assertFalse();
      expect(isSuccessfulProxyProbeResponse(400)).assertFalse();
    });
  });
}
