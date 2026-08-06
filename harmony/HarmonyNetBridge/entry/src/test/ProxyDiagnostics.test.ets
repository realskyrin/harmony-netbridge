import { describe, expect, it } from '@ohos/hypium';
import { isMitmInstallPage } from '../main/ets/vpn/ProxyDiagnostics';

export default function proxyDiagnosticsTest(): void {
  describe('ProxyDiagnostics', () => {
    it('acceptsMitmproxyInstallPage', 0, () => {
      expect(isMitmInstallPage(200, '<title>mitmproxy certificate install</title>')).assertTrue();
    });

    it('rejectsUnexpectedOrFailedResponse', 0, () => {
      expect(isMitmInstallPage(503, 'mitmproxy')).assertFalse();
      expect(isMitmInstallPage(200, '<html>unrelated</html>')).assertFalse();
    });
  });
}
