import { describe, expect, it } from '@ohos/hypium';
import { completeWithin } from '../main/ets/vpn/VpnStopTimeout';

export default function vpnStopTimeoutTest(): void {
  describe('VpnStopTimeout', () => {
    it('completesWhenTheOperationSettles', 0, async () => {
      await completeWithin(Promise.resolve(), 50, 'resolved operation');
      expect(true).assertTrue();
    });

    it('rejectsWhenTheOperationMissesItsDeadline', 0, async () => {
      const delayed: Promise<void> = new Promise<void>((resolve: () => void) => {
        setTimeout(() => {
          resolve();
        }, 50);
      });
      let message: string = '';
      try {
        await completeWithin(delayed, 1, 'stuck cleanup');
      } catch (error) {
        message = (error as Error).message;
      }
      expect(message.indexOf('stuck cleanup timed out') >= 0).assertTrue();
    });
  });
}
