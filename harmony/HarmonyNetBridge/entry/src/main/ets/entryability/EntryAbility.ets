import { AbilityConstant, UIAbility, Want } from '@kit.AbilityKit';
import { window } from '@kit.ArkUI';
import { hilog } from '@kit.PerformanceAnalysisKit';
import { bridgeSession } from '../bridge/BridgeSession';

const DOMAIN: number = 0x484E;
const TAG: string = 'HarmonyNetBridge';

export default class EntryAbility extends UIAbility {
  onCreate(_want: Want, _launchParam: AbilityConstant.LaunchParam): void {
    hilog.info(DOMAIN, TAG, 'EntryAbility created');
  }

  onWindowStageCreate(windowStage: window.WindowStage): void {
    windowStage.loadContent('pages/Index', (error) => {
      if (error.code) {
        hilog.error(DOMAIN, TAG, 'Failed to load Index. code=%{public}d', error.code);
        return;
      }
      hilog.info(DOMAIN, TAG, 'Index loaded; the Mac control session is ready');
    });
  }

  onDestroy(): void {
    bridgeSession.pauseAutoReconnect();
    bridgeSession.disconnect();
    hilog.info(DOMAIN, TAG, 'EntryAbility destroyed');
  }
}
