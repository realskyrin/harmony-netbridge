import { describe, expect, it } from '@ohos/hypium';
import {
  createDiagnosticDnsQuery,
  DnsTcpResponseDecoder,
  frameDnsOverTcp,
  validateDiagnosticDnsResponse
} from '../main/ets/vpn/DnsMessage';

function exactBuffer(bytes: Uint8Array): ArrayBuffer {
  return bytes.slice().buffer as ArrayBuffer;
}

function successfulResponse(): Uint8Array {
  return new Uint8Array([
    0x48, 0x4E, 0x81, 0x80,
    0x00, 0x01, 0x00, 0x01,
    0x00, 0x00, 0x00, 0x00
  ]);
}

export default function dnsMessageTest(): void {
  describe('DnsMessage', () => {
    it('buildsDiagnosticQuery', 0, () => {
      const query: Uint8Array = createDiagnosticDnsQuery();
      expect(query.length).assertEqual(29);
      expect(query[0]).assertEqual(0x48);
      expect(query[1]).assertEqual(0x4E);
      expect(query[5]).assertEqual(1);
      expect(query[query.length - 1]).assertEqual(1);
    });

    it('validatesSuccessfulResponse', 0, () => {
      expect(validateDiagnosticDnsResponse(successfulResponse())).assertEqual(1);
    });

    it('decodesFragmentedTcpResponse', 0, () => {
      const framed: Uint8Array = frameDnsOverTcp(successfulResponse());
      const decoder: DnsTcpResponseDecoder = new DnsTcpResponseDecoder();
      expect(decoder.push(exactBuffer(framed.slice(0, 5)))).assertUndefined();
      const decoded: Uint8Array | undefined = decoder.push(exactBuffer(framed.slice(5)));
      expect(decoded === undefined ? 0 : decoded.length).assertEqual(12);
      if (decoded !== undefined) {
        expect(validateDiagnosticDnsResponse(decoded)).assertEqual(1);
      }
    });

    it('rejectsDnsErrors', 0, () => {
      const response: Uint8Array = successfulResponse();
      response[3] = 0x83;
      let rejected: boolean = false;
      try {
        validateDiagnosticDnsResponse(response);
      } catch (error) {
        rejected = true;
      }
      expect(rejected).assertTrue();
    });
  });
}
