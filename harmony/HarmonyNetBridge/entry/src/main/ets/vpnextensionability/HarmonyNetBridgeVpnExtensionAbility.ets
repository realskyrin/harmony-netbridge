import { Want } from '@kit.AbilityKit';
import { BusinessError, commonEventManager } from '@kit.BasicServicesKit';
import { socket, vpnExtension, VpnExtensionAbility } from '@kit.NetworkKit';
import { hilog } from '@kit.PerformanceAnalysisKit';
import hnbNative, {
  DataConnectResult,
  GateProbeResult,
  PacketPumpStatus
} from 'libhnb.so';
import { encodeUtf8 } from '../bridge/HnbProtocol';
import {
  ALWAYS_TUNNELED_BUNDLE_NAME,
  applyVpnApplicationRouting,
  AppRoutingMode,
  buildVpnApplicationRouting,
  canStartAppRouting,
  decodeAppRoutingMode,
  decodeSelectedApplications,
  VpnApplicationRouting
} from '../vpn/AppWhitelist';
import { GateTunnel } from '../vpn/GateTunnel';
import { reconnectDelay } from '../vpn/ReconnectPolicy';
import { VpnControlHandshake, VpnControlSession } from '../vpn/VpnControlSession';
import { VPN_STOP_EVENT, VpnRuntimeState } from '../vpn/VpnRuntimeState';
import {
  completeWithin,
  VPN_CONTROL_STEP_TIMEOUT_MS,
  VPN_DESTROY_TIMEOUT_MS,
  VPN_STATUS_PUBLISH_TIMEOUT_MS
} from '../vpn/VpnStopTimeout';

const DOMAIN: number = 0x484E;
const TAG: string = 'HNBVpn';
const GATE_TAG: string = 'HNBGateV';
const BUNDLE_NAME: string = ALWAYS_TUNNELED_BUNDLE_NAME;
const EXTENSION_NAME: string = 'HarmonyNetBridgeVpnExtensionAbility';
const BRIDGE_ADDRESS: string = '127.0.0.1';
const BRIDGE_PORT: number = 27183;
const VIRTUAL_ADDRESS: string = '198.18.0.2';
const VIRTUAL_DNS: string = '198.18.0.1';
const DATA_HANDSHAKE_TIMEOUT_MS: number = 5000;
const PROBE_TIMEOUT_MS: number = 3000;
type UIStopResolve = (reason: string) => void;

// The extension is the sole owner of every Phase 4 socket, the VPN connection,
// and the native packet pump. EntryAbility only requests start/stop.
export default class HarmonyNetBridgeVpnExtensionAbility extends VpnExtensionAbility {
  private mode: string = 'vpn';
  private vpnConnection: vpnExtension.VpnConnection | undefined = undefined;
  private vpnCreated: boolean = false;

  private gateTunnel: GateTunnel | undefined = undefined;
  private probeSocket: socket.UDPSocket | undefined = undefined;

  private controlSession: VpnControlSession | undefined = undefined;
  private proxyEnabled: boolean = false;
  private appRoutingMode: AppRoutingMode = AppRoutingMode.WHITELIST;
  private selectedApplications: string[] = [];
  private pumpStarted: boolean = false;
  private destroyRequested: boolean = false;
  private cleanupPromise: Promise<void> | undefined = undefined;
  private uiStopPromise: Promise<string> | undefined = undefined;
  private uiStopResolve: UIStopResolve | undefined = undefined;
  private uiStopSubscriber: commonEventManager.CommonEventSubscriber | undefined = undefined;

  onCreate(want: Want): void {
    if (want.parameters !== undefined && want.parameters['mode'] !== undefined) {
      this.mode = want.parameters['mode'] as string;
    }
    if (this.mode === 'gate-v') {
      hilog.info(DOMAIN, GATE_TAG, 'GATE_V_START');
      this.runGateV();
      return;
    }
    const encodedRoutingMode: string = want.parameters === undefined ||
      want.parameters['routingMode'] === undefined ? '' : want.parameters['routingMode'] as string;
    const encodedApplications: string = want.parameters === undefined ||
      want.parameters['selectedApplications'] === undefined ? '' :
      want.parameters['selectedApplications'] as string;
    this.appRoutingMode = decodeAppRoutingMode(encodedRoutingMode);
    this.selectedApplications = decodeSelectedApplications(encodedApplications);
    if (!canStartAppRouting(this.appRoutingMode, this.selectedApplications)) {
      VpnRuntimeState.publish('FAILED', 'APP 分流配置无效，已拒绝创建 VPN', false);
      hilog.warn(DOMAIN, TAG, 'VPN_START_REJECTED invalid_app_routing');
      this.stopOwnExtension();
      return;
    }
    this.subscribeToUIStop();
    hilog.info(DOMAIN, TAG, 'PHASE3_START');
    this.runPhase3();
  }

  onDestroy(): void {
    this.destroyRequested = true;
    if (this.mode === 'gate-v') {
      this.cleanupGate();
      hilog.info(DOMAIN, GATE_TAG, 'GATE_V_EXTENSION_DESTROYED');
      return;
    }
    const resolve: UIStopResolve | undefined = this.uiStopResolve;
    this.uiStopResolve = undefined;
    if (resolve !== undefined) {
      resolve('system_stop');
    }
    hnbNative.stopPacketPump();
    this.cleanupPhase3(false);
    this.unsubscribeFromUIStop();
    hilog.info(DOMAIN, TAG, 'PHASE3_EXTENSION_DESTROYED');
  }

  private async runPhase3(): Promise<void> {
    let reconnectAttempt: number = 0;
    let terminalReason: string = 'system_stop';
    this.proxyEnabled = false;
    VpnRuntimeState.publish('STARTING', '正在建立 HNB Control/Data 通道', false);
    try {
      while (!this.destroyRequested) {
        try {
          const stopReason: string = await this.runVpnSession();
          reconnectAttempt = 0;
          if (stopReason.indexOf('transport_lost:') === 0 || stopReason.indexOf('pump_error:') === 0) {
            throw new Error(stopReason);
          }
          terminalReason = stopReason;
          hilog.info(DOMAIN, TAG, 'PHASE3_STOP_REQUEST reason=%{public}s', stopReason);
          break;
        } catch (error) {
          if (this.destroyRequested) {
            break;
          }
          const businessError: BusinessError = error as BusinessError;
          const code: number = businessError.code === undefined ? -1 : businessError.code;
          reconnectAttempt += 1;
          const controlSession: VpnControlSession | undefined = this.controlSession;
          if (controlSession !== undefined) {
            try {
              await controlSession.reportStatus('RECONNECTING', 'transport_recovery');
            } catch (statusError) {
              // A lost transport commonly makes this best-effort status fail.
            }
          }
          await this.cleanupPhase3(false);
          const delay: number = reconnectDelay(reconnectAttempt);
        VpnRuntimeState.publish('RECONNECTING',
          `连接中断，${delay / 1000} 秒后进行第 ${reconnectAttempt} 次重连`, this.proxyEnabled);
          hilog.warn(DOMAIN, TAG,
            'PHASE3_RECONNECT attempt=%{public}d delay_ms=%{public}d code=%{public}d message=%{public}s',
            reconnectAttempt, delay, code, businessError.message);
          const waitResult: string = await Promise.race([
            this.waitForRetry(delay),
            this.waitForUIStop()
          ]);
          if (waitResult !== 'retry') {
            terminalReason = waitResult;
            break;
          }
          VpnRuntimeState.publish('STARTING', `正在进行第 ${reconnectAttempt} 次自动重连`, this.proxyEnabled);
        }
      }
    } finally {
      const reportStopped: boolean = terminalReason === 'ui_requested' || terminalReason === 'user_requested' ||
        terminalReason === 'system_stop';
      await this.cleanupPhase3(reportStopped);
      this.unsubscribeFromUIStop();
      if (reportStopped) {
        try {
          await completeWithin(
            VpnRuntimeState.publishAndWait('STOPPED', 'VPN 已安全销毁', this.proxyEnabled),
            VPN_STATUS_PUBLISH_TIMEOUT_MS,
            'publish stopped status'
          );
        } catch (error) {
          const publishError: Error = error as Error;
          hilog.warn(DOMAIN, TAG, 'PHASE3_STOPPED_PUBLISH_INCOMPLETE message=%{public}s', publishError.message);
        }
      }
      await this.stopOwnExtension();
    }
  }

  private async runVpnSession(): Promise<string> {
    const vpnConnection: vpnExtension.VpnConnection = vpnExtension.createVpnConnection(this.context);
    this.vpnConnection = vpnConnection;
    const controlSession: VpnControlSession = new VpnControlSession();
    this.controlSession = controlSession;
    const handshake: VpnControlHandshake = await controlSession.connect();
    this.proxyEnabled = handshake.proxyEnabled;
    VpnRuntimeState.publish('STARTING', handshake.proxyEnabled ?
      'Mac 抓包代理已就绪，正在创建 VPN' : 'Mac 标准转发已就绪，正在创建 VPN', handshake.proxyEnabled);
    hilog.info(DOMAIN, TAG, 'PHASE3_CONTROL_CONNECTED mtu=%{public}d', handshake.mtu);
    await controlSession.reportStatus('STARTING', 'control_connected');

    const dataResult: DataConnectResult = hnbNative.connectData(
      BRIDGE_ADDRESS,
      BRIDGE_PORT,
      handshake.sessionToken,
      DATA_HANDSHAKE_TIMEOUT_MS
    );
    if (dataResult.status !== 0 || dataResult.fd < 0) {
      throw new Error(`Native data handshake failed: ${dataResult.status}`);
    }
    hilog.info(DOMAIN, TAG, 'PHASE3_DATA_CONNECTED');

    try {
      await vpnConnection.protect(handshake.socketFd);
      await vpnConnection.protect(dataResult.fd);
    } catch (error) {
      const protectError: BusinessError = error as BusinessError;
      throw new Error(`VPN socket protect failed: ${protectError.code}`);
    }
    hilog.info(DOMAIN, TAG, 'PHASE3_PROTECT_OK');

    const appRouting: VpnApplicationRouting = buildVpnApplicationRouting(
      this.appRoutingMode,
      this.selectedApplications
    );
    const config: vpnExtension.VpnConfig = {
      addresses: [
        {
          address: { address: VIRTUAL_ADDRESS, family: 1 },
          prefixLength: 30
        }
      ],
      routes: [
        {
          interface: 'vpn-tun',
          destination: {
            address: { address: '0.0.0.0', family: 1 },
            prefixLength: 0
          },
          gateway: { address: '0.0.0.0', family: 1 },
          hasGateway: false,
          isDefaultRoute: true
        }
      ],
      dnsAddresses: [VIRTUAL_DNS],
      mtu: handshake.mtu,
      isIPv4Accepted: true,
      isIPv6Accepted: false,
      isBlocking: false
    };
    // HarmonyOS models both App filters as optional. Only send the active
    // filter: some device builds select the whitelist path when the property
    // exists even if its value is an empty array, which can make a non-empty
    // blacklist ineffective and route excluded Apps into the VPN.
    applyVpnApplicationRouting(config, appRouting);
    let tunFd: number;
    try {
      tunFd = await vpnConnection.create(config);
    } catch (error) {
      const createError: BusinessError = error as BusinessError;
      throw new Error(`VPN create failed: ${createError.code}`);
    }
    this.vpnCreated = true;
    hilog.info(DOMAIN, TAG, 'PHASE3_VPN_CREATE_OK mtu=%{public}d routing_mode=%{public}s selected_apps=%{public}d',
      handshake.mtu, this.appRoutingMode, this.selectedApplications.length);

    const startResult: number = hnbNative.startPacketPump(tunFd);
    if (startResult !== 0) {
      throw new Error(`Native packet pump failed to start: ${startResult}`);
    }
    this.pumpStarted = true;
    const activeDetail: string = this.activeRoutingDetail(handshake.proxyEnabled, handshake.mtu);
    VpnRuntimeState.publish('ACTIVE', activeDetail, handshake.proxyEnabled);
    await controlSession.reportStatus('ACTIVE', 'packet_pump_started');
    hilog.info(DOMAIN, TAG, 'PHASE3_ACTIVE');
    if (handshake.proxyEnabled) {
      hilog.info(DOMAIN, TAG, 'PHASE4_PROXY_ACTIVE');
    }

    return await Promise.race([
      controlSession.waitForStop(),
      this.waitForPumpStop(),
      this.waitForUIStop()
    ]);
  }

  private activeRoutingDetail(proxyEnabled: boolean, mtu: number): string {
    const connectionDetail: string = proxyEnabled ? '已接入 Mac 抓包代理' : '已通过 USB 转发到 Mac';
    if (this.appRoutingMode === AppRoutingMode.BLACKLIST) {
      return this.selectedApplications.length === 0 ?
        `所有应用${connectionDetail} · MTU ${mtu}` :
        `除 ${this.selectedApplications.length} 个黑名单应用外，其余应用${connectionDetail} · MTU ${mtu}`;
    }
    return this.selectedApplications.length === 0 ?
      `本应用${connectionDetail} · MTU ${mtu}` :
      `本应用及 ${this.selectedApplications.length} 个白名单应用${connectionDetail} · MTU ${mtu}`;
  }

  private waitForPumpStop(): Promise<string> {
    return new Promise<string>((resolve: (reason: string) => void) => {
      const inspect = (): void => {
        const status: PacketPumpStatus = hnbNative.getPacketPumpStatus();
        if (!status.running) {
          if (this.destroyRequested) {
            resolve('system_stop');
          } else {
            resolve(`pump_error:${status.errorCode}`);
          }
          return;
        }
        setTimeout(inspect, 250);
      };
      setTimeout(inspect, 250);
    });
  }

  private waitForRetry(delay: number): Promise<string> {
    return new Promise<string>((resolve: (reason: string) => void) => {
      setTimeout(() => {
        resolve('retry');
      }, delay);
    });
  }

  private subscribeToUIStop(): void {
    this.uiStopPromise = new Promise<string>((resolve: UIStopResolve) => {
      this.uiStopResolve = resolve;
    });
    const subscriber: commonEventManager.CommonEventSubscriber = commonEventManager.createSubscriberSync({
      events: [VPN_STOP_EVENT],
      publisherBundleName: BUNDLE_NAME
    });
    this.uiStopSubscriber = subscriber;
    commonEventManager.subscribe(subscriber,
      (error: BusinessError, data: commonEventManager.CommonEventData) => {
        if (error || data.data !== 'user_requested') {
          return;
        }
        const resolve: UIStopResolve | undefined = this.uiStopResolve;
        this.uiStopResolve = undefined;
        if (resolve !== undefined) {
          resolve('ui_requested');
        }
      });
  }

  private waitForUIStop(): Promise<string> {
    const promise: Promise<string> | undefined = this.uiStopPromise;
    if (promise === undefined) {
      throw new Error('VPN UI stop subscription is unavailable');
    }
    return promise;
  }

  private unsubscribeFromUIStop(): void {
    const subscriber: commonEventManager.CommonEventSubscriber | undefined = this.uiStopSubscriber;
    this.uiStopSubscriber = undefined;
    this.uiStopResolve = undefined;
    if (subscriber !== undefined) {
      commonEventManager.unsubscribe(subscriber);
    }
  }

  private async cleanupPhase3(reportStopped: boolean): Promise<void> {
    if (this.cleanupPromise !== undefined) {
      await this.cleanupPromise;
      return;
    }
    const cleanup: Promise<void> = this.performPhase3Cleanup(reportStopped);
    this.cleanupPromise = cleanup;
    try {
      await cleanup;
    } finally {
      if (this.cleanupPromise === cleanup) {
        this.cleanupPromise = undefined;
      }
    }
  }

  private async performPhase3Cleanup(reportStopped: boolean): Promise<void> {
    hnbNative.stopPacketPump();
    this.pumpStarted = false;
    const vpnConnection: vpnExtension.VpnConnection | undefined = this.vpnConnection;
    const shouldDestroyVpn: boolean = this.vpnCreated && vpnConnection !== undefined;
    this.vpnCreated = false;
    this.vpnConnection = undefined;
    const controlSession: VpnControlSession | undefined = this.controlSession;
    this.controlSession = undefined;
    if (shouldDestroyVpn && vpnConnection !== undefined) {
      try {
        await completeWithin(vpnConnection.destroy(), VPN_DESTROY_TIMEOUT_MS, 'destroy VPN connection');
        hilog.info(DOMAIN, TAG, 'PHASE3_VPN_DESTROY_OK');
      } catch (error) {
        const destroyError: Error = error as Error;
        hilog.warn(DOMAIN, TAG, 'PHASE3_VPN_DESTROY_INCOMPLETE message=%{public}s', destroyError.message);
      }
    }
    if (controlSession !== undefined) {
      if (reportStopped) {
        try {
          await completeWithin(
            controlSession.reportStatus('STOPPED', 'vpn_destroyed'),
            VPN_CONTROL_STEP_TIMEOUT_MS,
            'report stopped status'
          );
        } catch (error) {
          // The Mac may already have closed its control socket.
        }
      }
      try {
        await completeWithin(controlSession.close(), VPN_CONTROL_STEP_TIMEOUT_MS, 'close control session');
      } catch (error) {
        const closeError: Error = error as Error;
        hilog.warn(DOMAIN, TAG, 'PHASE3_CONTROL_CLOSE_INCOMPLETE message=%{public}s', closeError.message);
      }
    }
  }

  // Gate V deliberately installs only one /32 route and immediately destroys
  // it after proving protect(), create(), native TUN read, and cleanup.
  private async runGateV(): Promise<void> {
    try {
      this.vpnConnection = vpnExtension.createVpnConnection(this.context);
      this.gateTunnel = new GateTunnel();
      const tunnelFd: number = await this.gateTunnel.connect();
      hilog.info(DOMAIN, GATE_TAG, 'GATE_V_TUNNEL_CONNECTED');

      await this.vpnConnection.protect(tunnelFd);
      hilog.info(DOMAIN, GATE_TAG, 'GATE_V_PROTECT_OK');

      const config: vpnExtension.VpnConfig = {
        addresses: [
          {
            address: { address: VIRTUAL_ADDRESS, family: 1 },
            prefixLength: 30
          }
        ],
        routes: [
          {
            interface: 'vpn-tun',
            destination: {
              address: { address: VIRTUAL_DNS, family: 1 },
              prefixLength: 32
            },
            gateway: { address: '0.0.0.0', family: 1 },
            hasGateway: false,
            isDefaultRoute: false
          }
        ],
        mtu: 1400,
        isIPv4Accepted: true,
        isIPv6Accepted: false,
        isBlocking: false,
        trustedApplications: [BUNDLE_NAME]
      };
      const tunFd: number = await this.vpnConnection.create(config);
      this.vpnCreated = true;
      hilog.info(DOMAIN, GATE_TAG, 'GATE_V_CREATE_OK');

      const probeSocket: socket.UDPSocket = socket.constructUDPSocketInstance();
      this.probeSocket = probeSocket;
      await probeSocket.bind({ address: '0.0.0.0', family: 1, port: 0 });
      const probePayload: Uint8Array = encodeUtf8('HarmonyNetBridge Gate V');
      await probeSocket.send({
        data: probePayload.buffer as ArrayBuffer,
        address: { address: VIRTUAL_DNS, family: 1, port: 9 }
      });

      const result: GateProbeResult = hnbNative.probeTun(tunFd, PROBE_TIMEOUT_MS);
      if (result.status !== 0 || result.version !== 4 || result.protocol !== 17 ||
        result.destination !== VIRTUAL_DNS || result.length < 28) {
        hilog.warn(DOMAIN, GATE_TAG,
          'GATE_V_PROBE_MISMATCH status=%{public}d length=%{public}d version=%{public}d protocol=%{public}d destination=%{public}s',
          result.status, result.length, result.version, result.protocol, result.destination);
        throw new Error(`TUN probe failed with status ${result.status}`);
      }
      hilog.info(DOMAIN, GATE_TAG, 'GATE_V_TUN_READ_OK length=%{public}d', result.length);

      await this.vpnConnection.destroy();
      this.vpnCreated = false;
      hilog.info(DOMAIN, GATE_TAG, 'GATE_V_DESTROY_OK');
      hilog.info(DOMAIN, GATE_TAG, 'GATE_V_PASS');
    } catch (error) {
      const businessError: BusinessError = error as BusinessError;
      const code: number = businessError.code === undefined ? -1 : businessError.code;
      hilog.error(DOMAIN, GATE_TAG, 'GATE_V_FAIL code=%{public}d message=%{public}s', code, businessError.message);
    } finally {
      await this.cleanupGate();
      await this.stopOwnExtension();
    }
  }

  private async cleanupGate(): Promise<void> {
    const probeSocket: socket.UDPSocket | undefined = this.probeSocket;
    this.probeSocket = undefined;
    if (probeSocket !== undefined) {
      try {
        await probeSocket.close();
      } catch (error) {
        // Cleanup is intentionally idempotent.
      }
    }
    if (this.vpnCreated && this.vpnConnection !== undefined) {
      try {
        await this.vpnConnection.destroy();
      } catch (error) {
        // The system may already have destroyed the Gate V VPN.
      }
      this.vpnCreated = false;
    }
    const gateTunnel: GateTunnel | undefined = this.gateTunnel;
    this.gateTunnel = undefined;
    if (gateTunnel !== undefined) {
      await gateTunnel.close();
    }
    this.vpnConnection = undefined;
  }

  private async stopOwnExtension(): Promise<void> {
    try {
      await vpnExtension.stopVpnExtensionAbility({
        bundleName: BUNDLE_NAME,
        abilityName: EXTENSION_NAME
      });
    } catch (error) {
      const stopError: BusinessError = error as BusinessError;
      hilog.warn(DOMAIN, this.mode === 'gate-v' ? GATE_TAG : TAG,
        'STOP_EXTENSION_FAIL code=%{public}d', stopError.code);
    }
  }
}
