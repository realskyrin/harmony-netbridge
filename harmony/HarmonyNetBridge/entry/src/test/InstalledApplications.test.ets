import { describe, expect, it } from '@ohos/hypium';
import {
  applicationDisplayName,
  decodeInstalledApplications,
  filterInstalledApplications,
  InstalledApplication,
  prioritizeSelectedApplications
} from '../main/ets/vpn/InstalledApplications';
import { ALWAYS_TUNNELED_BUNDLE_NAME } from '../main/ets/vpn/AppWhitelist';

export default function installedApplicationsTest(): void {
  describe('InstalledApplications', () => {
    it('decodesValidApplicationsAndRejectsMalformedEntries', 0, () => {
      const applications: InstalledApplication[] = decodeInstalledApplications(
        '{"applications":[' +
          '{"bundleName":"com.example.browser","label":"浏览器"},' +
          `{"bundleName":"${ALWAYS_TUNNELED_BUNDLE_NAME}","label":"HarmonyNetBridge"},` +
          '{"bundleName":"invalid","label":"无效"},' +
          '{"bundleName":"com.example.browser","label":"重复"},' +
          '{"bundleName":"com.example.mail","label":""}' +
        ']}');
      expect(applications.length).assertEqual(2);
      expect(applications[0].label).assertEqual('浏览器');
      expect(applications[1].bundleName).assertEqual('com.example.mail');
      expect(decodeInstalledApplications('not-json').length).assertEqual(0);
    });

    it('filtersByLabelAndBundleName', 0, () => {
      const applications: InstalledApplication[] = [
        { bundleName: 'com.example.browser', label: '浏览器' },
        { bundleName: 'com.example.mail', label: '邮箱' }
      ];
      expect(filterInstalledApplications(applications, '浏览').length).assertEqual(1);
      expect(filterInstalledApplications(applications, 'MAIL').length).assertEqual(1);
      expect(filterInstalledApplications(applications, '').length).assertEqual(2);
      expect(applicationDisplayName({ bundleName: 'com.example.reader', label: '' })).assertEqual('reader');
    });

    it('prioritizesSelectedApplicationsWithoutReorderingGroups', 0, () => {
      const applications: InstalledApplication[] = [
        { bundleName: 'com.example.browser', label: '浏览器' },
        { bundleName: 'com.example.mail', label: '邮箱' },
        { bundleName: 'com.example.maps', label: '地图' },
        { bundleName: 'com.example.reader', label: '阅读器' }
      ];
      const prioritized: InstalledApplication[] = prioritizeSelectedApplications(
        applications,
        ['com.example.maps', 'com.example.mail']
      );
      expect(prioritized.length).assertEqual(4);
      expect(prioritized[0].bundleName).assertEqual('com.example.mail');
      expect(prioritized[1].bundleName).assertEqual('com.example.maps');
      expect(prioritized[2].bundleName).assertEqual('com.example.browser');
      expect(prioritized[3].bundleName).assertEqual('com.example.reader');
      expect(applications[0].bundleName).assertEqual('com.example.browser');
    });
  });
}
