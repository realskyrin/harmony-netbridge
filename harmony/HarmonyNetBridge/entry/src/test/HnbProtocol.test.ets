import { describe, expect, it } from '@ohos/hypium';
import {
  decodeJsonPayload,
  decodeUtf8,
  encodeFrame,
  encodeJsonPayload,
  encodeUtf8,
  HelloPayload,
  HNB_HEADER_SIZE,
  HnbFrame,
  HnbFrameDecoder,
  HnbFrameType,
  isSupportedMtu,
  StopRequestPayload
} from '../main/ets/bridge/HnbProtocol';

function exactBuffer(bytes: Uint8Array): ArrayBuffer {
  return bytes.slice().buffer as ArrayBuffer;
}

function toHex(bytes: Uint8Array): string {
  const digits: string = '0123456789abcdef';
  let result: string = '';
  for (let index: number = 0; index < bytes.length; index++) {
    result += digits.charAt(Math.floor(bytes[index] / 16));
    result += digits.charAt(bytes[index] % 16);
  }
  return result;
}

export default function hnbProtocolTest(): void {
  describe('HnbProtocol', () => {
    it('encodesHelloHeader', 0, () => {
      const hello: HelloPayload = {
        role: 'control',
        mode: 'phase1',
        appVersion: 'development',
        supportedVersions: [1],
        capabilities: ['control'],
        message: 'hello'
      };
      const payload: Uint8Array = encodeJsonPayload(hello);
      const encoded: Uint8Array = encodeFrame(HnbFrameType.HELLO, 1, payload);
      expect(encoded[0]).assertEqual(0x48);
      expect(encoded[1]).assertEqual(0x4E);
      expect(encoded[2]).assertEqual(0x42);
      expect(encoded[3]).assertEqual(0x31);
      expect(encoded[4]).assertEqual(1);
      expect(encoded[5]).assertEqual(HnbFrameType.HELLO);
      expect(encoded.length).assertEqual(HNB_HEADER_SIZE + payload.length);
      // Canonical source: ../../../../../testdata/hnb1-phase1-hello.json
      expect(toHex(encoded)).assertEqual(
        '484e42310101000000000082000000017b22726f6c65223a22636f6e74726f6c222c226d6f6465223a22706861736531222c2261707056657273696f6e223a22646576656c6f706d656e74222c22737570706f7274656456657273696f6e73223a5b315d2c226361706162696c6974696573223a5b22636f6e74726f6c225d2c226d657373616765223a2268656c6c6f227d'
      );
    });

    it('decodesFragmentedAndCoalescedFrames', 0, () => {
      const first: Uint8Array = encodeFrame(HnbFrameType.STOP_ACK, 1, new Uint8Array(0));
      const stopRequest: StopRequestPayload = { reason: 'test' };
      const secondPayload: Uint8Array = encodeJsonPayload(stopRequest);
      expect(secondPayload.length).assertLarger(0);
      const second: Uint8Array = encodeFrame(HnbFrameType.STOP_REQUEST, 2, secondPayload);
      const joined: Uint8Array = new Uint8Array(first.length + second.length);
      joined.set(first, 0);
      joined.set(second, first.length);

      const decoder: HnbFrameDecoder = new HnbFrameDecoder();
      const incomplete: HnbFrame[] = decoder.push(exactBuffer(joined.slice(0, 9)));
      expect(incomplete.length).assertEqual(0);
      const complete: HnbFrame[] = decoder.push(exactBuffer(joined.slice(9)));
      expect(complete.length).assertEqual(2);
      expect(complete[0].type).assertEqual(HnbFrameType.STOP_ACK);
      expect(complete[1].type).assertEqual(HnbFrameType.STOP_REQUEST);
      expect(complete[1].payload.length).assertEqual(secondPayload.length);
      const request: Object = decodeJsonPayload<Object>(complete[1].payload);
      expect(JSON.stringify(request)).assertEqual('{"reason":"test"}');
    });

    it('rejectsInvalidMagic', 0, () => {
      const encoded: Uint8Array = encodeFrame(HnbFrameType.STOP_ACK, 1, new Uint8Array(0));
      encoded[0] = 0;
      const decoder: HnbFrameDecoder = new HnbFrameDecoder();
      let rejected: boolean = false;
      try {
        decoder.push(exactBuffer(encoded));
      } catch (error) {
        rejected = true;
      }
      expect(rejected).assertTrue();
    });

    it('roundTripsUtf8ControlText', 0, () => {
      const text: string = '鸿蒙 USB bridge 🌉';
      const encoded: Uint8Array = encodeUtf8(text);
      expect(encoded.length).assertLarger(text.length);
      expect(decodeUtf8(encoded)).assertEqual(text);
    });

    it('rejectsMalformedUtf8', 0, () => {
      let rejected: boolean = false;
      try {
        decodeUtf8(new Uint8Array([0xE2, 0x82]));
      } catch (error) {
        rejected = true;
      }
      expect(rejected).assertTrue();
    });

    it('validatesNegotiatedMtu', 0, () => {
      expect(isSupportedMtu(576)).assertTrue();
      expect(isSupportedMtu(1280)).assertTrue();
      expect(isSupportedMtu(1500)).assertTrue();
      expect(isSupportedMtu(575)).assertFalse();
      expect(isSupportedMtu(1501)).assertFalse();
      expect(isSupportedMtu(1280.5)).assertFalse();
    });
  });
}
