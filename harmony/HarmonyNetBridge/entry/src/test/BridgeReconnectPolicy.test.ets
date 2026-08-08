import { describe, expect, it } from '@ohos/hypium';
import {
  bridgeReconnectDelay,
  shouldScheduleBridgeReconnect
} from '../main/ets/bridge/BridgeReconnectPolicy';

export default function bridgeReconnectPolicyTest(): void {
  describe('BridgeReconnectPolicy', () => {
    it('usesResponsiveBoundedBackoff', 0, () => {
      expect(bridgeReconnectDelay(1)).assertEqual(500);
      expect(bridgeReconnectDelay(2)).assertEqual(1000);
      expect(bridgeReconnectDelay(3)).assertEqual(2000);
      expect(bridgeReconnectDelay(4)).assertEqual(4000);
      expect(bridgeReconnectDelay(20)).assertEqual(4000);
    });

    it('retriesOnlyWhenTheControlConnectionIsIdle', 0, () => {
      expect(shouldScheduleBridgeReconnect(true, false, 'DISCONNECTED')).assertTrue();
      expect(shouldScheduleBridgeReconnect(true, false, 'ERROR')).assertTrue();
      expect(shouldScheduleBridgeReconnect(false, false, 'DISCONNECTED')).assertFalse();
      expect(shouldScheduleBridgeReconnect(true, true, 'DISCONNECTED')).assertFalse();
      expect(shouldScheduleBridgeReconnect(true, false, 'CONNECTING')).assertFalse();
      expect(shouldScheduleBridgeReconnect(true, false, 'CONNECTED')).assertFalse();
    });
  });
}
