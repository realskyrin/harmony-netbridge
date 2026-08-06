import { Want } from '@kit.AbilityKit';
import { VpnExtensionAbility } from '@kit.NetworkKit';
import { hilog } from '@kit.PerformanceAnalysisKit';

const DOMAIN: number = 0x484E42;
const TAG: string = 'HNBVpnSkeleton';

// Phase 1 registers the official VPN extension type but deliberately does not
// request authorization, create routes, or call vpn.create(). Gate V will add a
// narrowly scoped real-device probe before any Phase 2 implementation.
export default class HarmonyNetBridgeVpnExtensionAbility extends VpnExtensionAbility {
  onCreate(_want: Want): void {
    hilog.info(DOMAIN, TAG, 'VpnExtensionAbility skeleton created; VPN remains stopped');
  }

  onDestroy(): void {
    hilog.info(DOMAIN, TAG, 'VpnExtensionAbility skeleton destroyed');
  }
}
