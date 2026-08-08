import { describe, expect, it } from '@ohos/hypium';
import {
  decodeTrustedApplications,
  isValidBundleName,
  normalizeBundleName,
  normalizeTrustedApplications
} from '../main/ets/vpn/AppWhitelist';

export default function appWhitelistTest(): void {
  describe('AppWhitelist', () => {
    it('validatesHarmonyBundleNames', 0, () => {
      expect(isValidBundleName('com.example.browser')).assertTrue();
      expect(isValidBundleName('io.github.realskyrin.app_2')).assertTrue();
      expect(isValidBundleName('com..browser')).assertFalse();
      expect(isValidBundleName('1com.example.browser')).assertFalse();
      expect(isValidBundleName('com.example._browser')).assertFalse();
      expect(isValidBundleName('com.example.browser-2')).assertFalse();
    });

    it('normalizesAndDeduplicatesEntries', 0, () => {
      expect(normalizeBundleName('  com.example.browser  ')).assertEqual('com.example.browser');
      const normalized: string[] = normalizeTrustedApplications([
        ' com.example.browser ',
        'com.example.browser',
        'invalid',
        'com.example.mail'
      ]);
      expect(normalized.length).assertEqual(2);
      expect(normalized[0]).assertEqual('com.example.browser');
      expect(normalized[1]).assertEqual('com.example.mail');
    });

    it('failsClosedForMalformedStartParameters', 0, () => {
      expect(decodeTrustedApplications('not-json').length).assertEqual(0);
      expect(decodeTrustedApplications('{}').length).assertEqual(0);
      const decoded: string[] = decodeTrustedApplications('["com.example.browser","invalid"]');
      expect(decoded.length).assertEqual(1);
      expect(decoded[0]).assertEqual('com.example.browser');
    });
  });
}
