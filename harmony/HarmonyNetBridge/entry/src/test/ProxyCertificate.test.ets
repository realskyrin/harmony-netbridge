import { describe, expect, it } from '@ohos/hypium';
import { isCACertificateDownload } from '../main/ets/vpn/ProxyCertificate';

function pemFixture(): Uint8Array {
  const marker: string = '-----BEGIN CERTIFICATE-----';
  const payload: Uint8Array = new Uint8Array(512);
  for (let index: number = 0; index < marker.length; index++) {
    payload[index] = marker.charCodeAt(index);
  }
  return payload;
}

export default function proxyCertificateTest(): void {
  describe('ProxyCertificate', () => {
    it('acceptsBoundedPemAndDerCertificates', 0, () => {
      expect(isCACertificateDownload(200, pemFixture())).assertTrue();
      const der: Uint8Array = new Uint8Array(512);
      der[0] = 0x30;
      expect(isCACertificateDownload(200, der)).assertTrue();
    });

    it('rejectsHttpErrorsHtmlAndUnboundedPayloads', 0, () => {
      expect(isCACertificateDownload(503, pemFixture())).assertFalse();
      expect(isCACertificateDownload(200, new Uint8Array(512))).assertFalse();
      expect(isCACertificateDownload(200, new Uint8Array(255))).assertFalse();
      const oversized: Uint8Array = new Uint8Array(64 * 1024 + 1);
      oversized[0] = 0x30;
      expect(isCACertificateDownload(200, oversized)).assertFalse();
    });
  });
}
