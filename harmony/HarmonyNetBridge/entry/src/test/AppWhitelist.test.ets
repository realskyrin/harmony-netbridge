import { describe, expect, it } from '@ohos/hypium';
import {
  ALWAYS_TUNNELED_BUNDLE_NAME,
  AppRoutingMode,
  applicationUsesTunnel,
  buildVpnApplicationRouting,
  canStartAppRouting,
  decodeAppRoutingMode,
  decodeSelectedApplications,
  isValidBundleName,
  normalizeBundleName,
  normalizeSelectedApplications,
  VpnApplicationRouting
} from '../main/ets/vpn/AppWhitelist';

export default function appWhitelistTest(): void {
  describe('AppRouting', () => {
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
      const normalized: string[] = normalizeSelectedApplications([
        ' com.example.browser ',
        'com.example.browser',
        ALWAYS_TUNNELED_BUNDLE_NAME,
        'invalid',
        'com.example.mail'
      ]);
      expect(normalized.length).assertEqual(2);
      expect(normalized[0]).assertEqual('com.example.browser');
      expect(normalized[1]).assertEqual('com.example.mail');
    });

    it('failsClosedForMalformedStartParameters', 0, () => {
      expect(decodeSelectedApplications('not-json').length).assertEqual(0);
      expect(decodeSelectedApplications('{}').length).assertEqual(0);
      const decoded: string[] = decodeSelectedApplications(
        `["${ALWAYS_TUNNELED_BUNDLE_NAME}","com.example.browser","invalid"]`);
      expect(decoded.length).assertEqual(1);
      expect(decoded[0]).assertEqual('com.example.browser');
      expect(decodeAppRoutingMode('unexpected')).assertEqual(AppRoutingMode.WHITELIST);
    });

    it('usesOneSelectionForBothRoutingModes', 0, () => {
      const selected: string[] = [
        'com.example.browser',
        ALWAYS_TUNNELED_BUNDLE_NAME,
        'com.example.mail'
      ];
      const whitelist: VpnApplicationRouting = buildVpnApplicationRouting(AppRoutingMode.WHITELIST, selected);
      expect(whitelist.trustedApplications.length).assertEqual(3);
      expect(whitelist.trustedApplications[0]).assertEqual(ALWAYS_TUNNELED_BUNDLE_NAME);
      expect(whitelist.blockedApplications.length).assertEqual(0);
      const blacklist: VpnApplicationRouting = buildVpnApplicationRouting(AppRoutingMode.BLACKLIST, selected);
      expect(blacklist.trustedApplications.length).assertEqual(0);
      expect(blacklist.blockedApplications.length).assertEqual(2);
      expect(blacklist.blockedApplications[0]).assertEqual('com.example.browser');
      expect(blacklist.blockedApplications.indexOf(ALWAYS_TUNNELED_BUNDLE_NAME)).assertEqual(-1);
    });

    it('definesEmptySelectionByMode', 0, () => {
      expect(canStartAppRouting(AppRoutingMode.WHITELIST, [])).assertTrue();
      expect(canStartAppRouting(AppRoutingMode.BLACKLIST, [])).assertTrue();
      const whitelist: VpnApplicationRouting = buildVpnApplicationRouting(AppRoutingMode.WHITELIST, []);
      expect(whitelist.trustedApplications.length).assertEqual(1);
      expect(whitelist.trustedApplications[0]).assertEqual(ALWAYS_TUNNELED_BUNDLE_NAME);
    });

    it('detectsWhetherAnApplicationUsesTunnel', 0, () => {
      const selected: string[] = ['com.example.browser'];
      expect(applicationUsesTunnel(AppRoutingMode.WHITELIST, selected, 'com.example.browser')).assertTrue();
      expect(applicationUsesTunnel(AppRoutingMode.WHITELIST, selected, 'com.example.mail')).assertFalse();
      expect(applicationUsesTunnel(AppRoutingMode.BLACKLIST, selected, 'com.example.browser')).assertFalse();
      expect(applicationUsesTunnel(AppRoutingMode.BLACKLIST, selected, 'com.example.mail')).assertTrue();
      expect(applicationUsesTunnel(AppRoutingMode.WHITELIST, selected,
        ALWAYS_TUNNELED_BUNDLE_NAME)).assertTrue();
      expect(applicationUsesTunnel(AppRoutingMode.BLACKLIST, selected,
        ALWAYS_TUNNELED_BUNDLE_NAME)).assertTrue();
    });
  });
}
