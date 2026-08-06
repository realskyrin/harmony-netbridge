import { describe, expect, it } from '@ohos/hypium';
import { reconnectDelay } from '../main/ets/vpn/ReconnectPolicy';

export default function reconnectPolicyTest(): void {
  describe('ReconnectPolicy', () => {
    it('usesBoundedExponentialBackoff', 0, () => {
      expect(reconnectDelay(1)).assertEqual(1000);
      expect(reconnectDelay(2)).assertEqual(2000);
      expect(reconnectDelay(3)).assertEqual(4000);
      expect(reconnectDelay(4)).assertEqual(8000);
      expect(reconnectDelay(5)).assertEqual(10000);
      expect(reconnectDelay(20)).assertEqual(10000);
    });
  });
}
