import { describe, expect, it } from '@ohos/hypium';
import {
  applicationDisplayName,
  decodeInstalledApplications,
  filterInstalledApplications,
  InstalledApplication
} from '../main/ets/vpn/InstalledApplications';

export default function installedApplicationsTest(): void {
  describe('InstalledApplications', () => {
    it('decodesValidApplicationsAndRejectsMalformedEntries', 0, () => {
      const applications: InstalledApplication[] = decodeInstalledApplications(
        '{"applications":[' +
          '{"bundleName":"com.example.browser","label":"浏览器"},' +
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
  });
}
