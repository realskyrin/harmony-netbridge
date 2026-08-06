import { commonEventManager } from '@kit.BasicServicesKit';

export const VPN_STATUS_EVENT: string = 'io.github.realskyrin.harmonynetbridge.event.VPN_STATUS';
export const VPN_STOP_EVENT: string = 'io.github.realskyrin.harmonynetbridge.event.VPN_STOP';
const BUNDLE_NAME: string = 'io.github.realskyrin.harmonynetbridge';

export class VpnRuntimeSnapshot {
  readonly state: string;
  readonly detail: string;
  readonly proxyEnabled: boolean;

  constructor(state: string, detail: string, proxyEnabled: boolean) {
    this.state = state;
    this.detail = detail;
    this.proxyEnabled = proxyEnabled;
  }
}

interface VpnRuntimeEventPayload {
  state: string;
  detail: string;
  proxyEnabled: boolean;
}

// HarmonyOS may host the VPN Extension separately from the UI Ability. Keep a
// process-local snapshot for rendering and mirror updates through a custom
// common event restricted to this bundle. The Mac CLI remains authoritative
// for cross-device status.
export class VpnRuntimeState {
  private static current: VpnRuntimeSnapshot = new VpnRuntimeSnapshot('STOPPED', 'VPN 尚未启动', false);

  static get snapshot(): VpnRuntimeSnapshot {
    return VpnRuntimeState.current;
  }

  static publish(state: string, detail: string,
    proxyEnabled: boolean = VpnRuntimeState.current.proxyEnabled): void {
    VpnRuntimeState.current = new VpnRuntimeSnapshot(state, detail, proxyEnabled);
    const payload: VpnRuntimeEventPayload = { state: state, detail: detail, proxyEnabled: proxyEnabled };
    commonEventManager.publish(VPN_STATUS_EVENT, {
      bundleName: BUNDLE_NAME,
      data: JSON.stringify(payload)
    }, () => {
      // Status delivery is best effort; the Mac control channel remains the
      // source of truth if the foreground Ability is not running.
    });
  }

  static acceptEvent(data: string | undefined): boolean {
    if (data === undefined || data.length > 1024) {
      return false;
    }
    try {
      const payload: VpnRuntimeEventPayload = JSON.parse(data) as VpnRuntimeEventPayload;
      if (payload.state.length === 0 || payload.state.length > 32 || payload.detail.length > 512 ||
        typeof payload.proxyEnabled !== 'boolean') {
        return false;
      }
      VpnRuntimeState.current = new VpnRuntimeSnapshot(payload.state, payload.detail, payload.proxyEnabled);
      return true;
    } catch (error) {
      return false;
    }
  }

  static requestStop(): void {
    commonEventManager.publish(VPN_STOP_EVENT, {
      bundleName: BUNDLE_NAME,
      data: 'user_requested'
    }, () => {
      // The Extension reports the terminal state through VPN_STATUS_EVENT.
    });
  }
}
