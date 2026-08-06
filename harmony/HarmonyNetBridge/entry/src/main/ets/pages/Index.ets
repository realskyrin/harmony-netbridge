import { common } from '@kit.AbilityKit';
import { BusinessError, commonEventManager } from '@kit.BasicServicesKit';
import { vpnExtension } from '@kit.NetworkKit';
import {
  BridgeConnectionState,
  BridgeSnapshot,
  bridgeSession
} from '../bridge/BridgeSession';
import { VpnDiagnosticResult, VpnNetworkDiagnostics } from '../vpn/VpnNetworkDiagnostics';
import { ProxyCertificate } from '../vpn/ProxyCertificate';
import { ProxyDiagnostics } from '../vpn/ProxyDiagnostics';
import { VPN_STATUS_EVENT, VpnRuntimeSnapshot, VpnRuntimeState } from '../vpn/VpnRuntimeState';

const BUNDLE_NAME: string = 'io.github.realskyrin.harmonynetbridge';
const EXTENSION_NAME: string = 'HarmonyNetBridgeVpnExtensionAbility';

@Entry
@Component
struct Index {
  @State connectionState: BridgeConnectionState = bridgeSession.snapshot.state;
  @State statusTitle: string = bridgeSession.snapshot.title;
  @State statusDetail: string = bridgeSession.snapshot.detail;
  @State gateStatus: string = 'NOT_RUN';
  @State gateDetail: string = '仅路由 198.18.0.1/32，完成后立即销毁 VPN';
  @State vpnState: string = VpnRuntimeState.snapshot.state;
  @State vpnDetail: string = VpnRuntimeState.snapshot.detail;
  @State proxyEnabled: boolean = VpnRuntimeState.snapshot.proxyEnabled;
  @State diagnosticState: string = 'NOT_RUN';
  @State diagnosticDetail: string = '启动 VPN 后可验证真实的 UDP DNS 与 TCP DNS 往返';
  @State proxyDiagnosticState: string = 'NOT_RUN';
  @State proxyDiagnosticDetail: string = '在 Mac 运行 proxy 后，可验证 HTTP 流量是否进入 mitmweb';
  @State certificateState: string = 'NOT_DOWNLOADED';
  @State certificateDetail: string = '从 Mac 本地 CA 经 USB 下载，无需访问外部证书站点';
  @State certificateDownloaded: boolean = false;
  private proxyCertificate: ProxyCertificate = new ProxyCertificate();
  private vpnStatusTimer: number = -1;
  private vpnStatusSubscriber: commonEventManager.CommonEventSubscriber | undefined = undefined;

  aboutToAppear(): void {
    bridgeSession.setListener((snapshot: BridgeSnapshot) => {
      this.connectionState = snapshot.state;
      this.statusTitle = snapshot.title;
      this.statusDetail = snapshot.detail;
    });
    this.subscribeVpnStatus();
    this.refreshVpnStatus();
    this.refreshCertificateStatus();
    this.vpnStatusTimer = setInterval(() => {
      this.refreshVpnStatus();
    }, 500);
  }

  aboutToDisappear(): void {
    bridgeSession.setListener(undefined);
    if (this.vpnStatusTimer >= 0) {
      clearInterval(this.vpnStatusTimer);
      this.vpnStatusTimer = -1;
    }
    const subscriber: commonEventManager.CommonEventSubscriber | undefined = this.vpnStatusSubscriber;
    this.vpnStatusSubscriber = undefined;
    if (subscriber !== undefined) {
      commonEventManager.unsubscribe(subscriber);
    }
  }

  private statusColor(): string {
    if (this.vpnOwnsUsbBridge()) {
      return this.vpnState === 'ACTIVE' ? '#22C55E' : '#38BDF8';
    }
    if (this.connectionState === BridgeConnectionState.CONNECTED) {
      return '#22C55E';
    }
    if (this.connectionState === BridgeConnectionState.ERROR) {
      return '#F87171';
    }
    if (this.connectionState === BridgeConnectionState.CONNECTING) {
      return '#38BDF8';
    }
    return '#94A3B8';
  }

  private vpnOwnsUsbBridge(): boolean {
    return this.vpnState === 'AUTH_REQUIRED' || this.vpnState === 'STARTING' ||
      this.vpnState === 'RECONNECTING' || this.vpnState === 'ACTIVE' || this.vpnState === 'STOPPING';
  }

  private displayedConnectionState(): string {
    return this.vpnOwnsUsbBridge() ? `VPN_${this.vpnState}` : this.connectionState;
  }

  private displayedStatusTitle(): string {
    if (this.vpnState === 'ACTIVE') {
      return this.proxyEnabled ? 'Mac USB 抓包通道已接管' : 'Mac USB 通道已接管';
    }
    if (this.vpnState === 'RECONNECTING') {
      return '正在重建 Mac USB 通道';
    }
    if (this.vpnOwnsUsbBridge()) {
      return 'VPN Extension 正在管理通道';
    }
    return this.statusTitle;
  }

  private displayedStatusDetail(): string {
    if (this.vpnOwnsUsbBridge()) {
      if (this.proxyEnabled) {
        return 'HTTP(S) TCP 由 Mac mitmweb 接管；HTTPS 解密需要手动信任 CA';
      }
      return 'Control / Data 双通道由 VPN Extension 管理，无需另建 Phase 1 会话';
    }
    return this.statusDetail;
  }

  private requestConnect(): void {
    bridgeSession.connect();
  }

  private requestDisconnect(): void {
    bridgeSession.disconnect();
  }

  private async requestGateV(): Promise<void> {
    this.gateStatus = 'STARTING';
    this.gateDetail = '正在关闭 Phase 1 会话并请求系统 VPN 授权';
    try {
      await bridgeSession.disconnect();
      await vpnExtension.startVpnExtensionAbility({
        bundleName: BUNDLE_NAME,
        abilityName: EXTENSION_NAME,
        parameters: { mode: 'gate-v' }
      });
      this.gateStatus = 'REQUESTED';
      this.gateDetail = '请确认系统 VPN 授权；结果同时写入 HNBGateV 日志';
    } catch (error) {
      const businessError: BusinessError = error as BusinessError;
      this.gateStatus = 'ERROR';
      this.gateDetail = `启动 Gate V 失败：${businessError.code}`;
    }
  }

  private async requestVpnStart(): Promise<void> {
    VpnRuntimeState.publish('STARTING', '正在关闭 Phase 1 会话并启动 VPN', false);
    this.refreshVpnStatus();
    try {
      await bridgeSession.disconnect();
      await vpnExtension.startVpnExtensionAbility({
        bundleName: BUNDLE_NAME,
        abilityName: EXTENSION_NAME,
        parameters: { mode: 'vpn' }
      });
    } catch (error) {
      const businessError: BusinessError = error as BusinessError;
      VpnRuntimeState.publish('FAILED', `启动 VPN 失败：${businessError.code}`, false);
      this.refreshVpnStatus();
    }
  }

  private requestVpnStop(): void {
    VpnRuntimeState.publish('STOPPING', '正在停止 PacketPump 并销毁 VPN');
    this.refreshVpnStatus();
    VpnRuntimeState.requestStop();
  }

  private refreshVpnStatus(): void {
    const snapshot: VpnRuntimeSnapshot = VpnRuntimeState.snapshot;
    this.vpnState = snapshot.state;
    this.vpnDetail = snapshot.detail;
    this.proxyEnabled = snapshot.proxyEnabled;
    if (this.proxyDiagnosticState === 'NOT_RUN') {
      this.proxyDiagnosticDetail = snapshot.proxyEnabled ?
        'mitmweb 已就绪；点击下方按钮验证 HTTP 抓包链路' :
        '在 Mac 运行 proxy 后，可验证 HTTP 流量是否进入 mitmweb';
    }
  }

  private subscribeVpnStatus(): void {
    if (this.vpnStatusSubscriber !== undefined) {
      return;
    }
    const subscriber: commonEventManager.CommonEventSubscriber = commonEventManager.createSubscriberSync({
      events: [VPN_STATUS_EVENT],
      publisherBundleName: BUNDLE_NAME
    });
    this.vpnStatusSubscriber = subscriber;
    commonEventManager.subscribe(subscriber,
      (error: BusinessError, data: commonEventManager.CommonEventData) => {
        if (error) {
          return;
        }
        if (VpnRuntimeState.acceptEvent(data.data)) {
          this.refreshVpnStatus();
        }
      });
  }

  private async requestNetworkDiagnostics(): Promise<void> {
    if (this.vpnState !== 'ACTIVE' || this.diagnosticState === 'RUNNING') {
      return;
    }
    this.diagnosticState = 'RUNNING';
    this.diagnosticDetail = '正在通过 TUN → USB → Mac Relay 查询 example.com';
    try {
      const result: VpnDiagnosticResult = await new VpnNetworkDiagnostics().run();
      this.diagnosticState = 'PASS';
      this.diagnosticDetail = result.detail;
    } catch (error) {
      const diagnosticError: Error = error as Error;
      this.diagnosticState = 'FAIL';
      this.diagnosticDetail = diagnosticError.message;
    }
  }

  private async requestProxyDiagnostics(): Promise<void> {
    if (this.vpnState !== 'ACTIVE' || !this.proxyEnabled || this.proxyDiagnosticState === 'RUNNING') {
      return;
    }
    this.proxyDiagnosticState = 'RUNNING';
    this.proxyDiagnosticDetail = '正在请求 http://mitm.it，验证 TUN → USB → mitmweb';
    try {
      this.proxyDiagnosticDetail = await new ProxyDiagnostics().run();
      this.proxyDiagnosticState = 'PASS';
    } catch (error) {
      const diagnosticError: Error = error as Error;
      this.proxyDiagnosticState = 'FAIL';
      this.proxyDiagnosticDetail = diagnosticError.message;
    }
  }

  private uiAbilityContext(): common.UIAbilityContext {
    return this.getUIContext().getHostContext() as common.UIAbilityContext;
  }

  private refreshCertificateStatus(): void {
    this.certificateDownloaded = this.proxyCertificate.hasDownloaded(this.uiAbilityContext());
    if (this.certificateDownloaded) {
      this.certificateState = 'READY';
      this.certificateDetail = 'mitmproxy-ca-cert.cer 已保存到本机，可交给系统证书管理器';
    }
  }

  private async requestCertificateDownload(): Promise<void> {
    if (this.vpnState !== 'ACTIVE' || !this.proxyEnabled || this.certificateState === 'DOWNLOADING') {
      return;
    }
    this.certificateState = 'DOWNLOADING';
    this.certificateDetail = '正在通过 hdc USB 通道读取 Mac 本地 CA';
    try {
      const bytes: number = await this.proxyCertificate.download(this.uiAbilityContext());
      this.certificateDownloaded = true;
      this.certificateState = 'READY';
      this.certificateDetail = `已保存 mitmproxy-ca-cert.cer（${bytes} 字节）`;
    } catch (error) {
      const certificateError: Error = error as Error;
      this.certificateDownloaded = this.proxyCertificate.hasDownloaded(this.uiAbilityContext());
      this.certificateState = 'FAIL';
      this.certificateDetail = `证书下载失败：${certificateError.message}`;
    }
  }

  private async requestOpenCertificateManager(): Promise<void> {
    if (!this.certificateDownloaded || this.certificateState === 'OPENING') {
      return;
    }
    this.certificateState = 'OPENING';
    this.certificateDetail = '正在打开系统证书管理器';
    try {
      await this.proxyCertificate.openWithCertificateManager(this.uiAbilityContext());
      this.certificateState = 'READY';
      this.certificateDetail = '证书管理器已打开；是否安装与信任以系统确认结果为准';
    } catch (error) {
      const certificateError: Error = error as Error;
      this.certificateState = 'FAIL';
      this.certificateDetail = `无法打开证书管理器：${certificateError.message}`;
    }
  }

  build() {
    Scroll() {
      Column({ space: 20 }) {
        Column({ space: 8 }) {
          Text('HarmonyNetBridge')
            .fontSize(30)
            .fontWeight(FontWeight.Bold)
            .fontColor('#F8FAFC')
          Text('Phase 4 · USB IPv4 VPN 与抓包')
            .fontSize(14)
            .fontColor('#94A3B8')
        }
        .width('100%')
        .alignItems(HorizontalAlign.Start)

        Column({ space: 14 }) {
          Row({ space: 10 }) {
            Text('●')
              .fontSize(13)
              .fontColor(this.statusColor())
            Text(this.displayedConnectionState())
              .fontSize(13)
              .fontWeight(FontWeight.Medium)
              .fontColor(this.statusColor())
          }
          .width('100%')

          Text(this.displayedStatusTitle())
            .fontSize(22)
            .fontWeight(FontWeight.Bold)
            .fontColor('#F8FAFC')
            .width('100%')

          Text(this.displayedStatusDetail())
            .fontSize(15)
            .fontColor('#CBD5E1')
            .lineHeight(23)
            .width('100%')

          if (!this.vpnOwnsUsbBridge()) {
            Row({ space: 12 }) {
              Button('连接 Mac')
                .enabled(this.connectionState !== BridgeConnectionState.CONNECTING &&
                  this.connectionState !== BridgeConnectionState.CONNECTED)
                .backgroundColor('#2563EB')
                .fontColor('#FFFFFF')
                .layoutWeight(1)
                .onClick(() => {
                  this.requestConnect();
                })

              Button('断开')
                .enabled(this.connectionState === BridgeConnectionState.CONNECTED ||
                  this.connectionState === BridgeConnectionState.CONNECTING)
                .backgroundColor('#334155')
                .fontColor('#F8FAFC')
                .layoutWeight(1)
                .onClick(() => {
                  this.requestDisconnect();
                })
            }
            .width('100%')
          }
        }
        .width('100%')
        .padding(20)
        .backgroundColor('#111C2E')
        .borderRadius(18)

        Column({ space: 12 }) {
          Text('Phase 4 抓包')
            .fontSize(18)
            .fontWeight(FontWeight.Bold)
            .fontColor('#F8FAFC')
            .width('100%')
          Text(this.proxyEnabled ? this.proxyDiagnosticState : 'OFF')
            .fontSize(13)
            .fontWeight(FontWeight.Medium)
            .fontColor(this.proxyDiagnosticState === 'PASS' ? '#22C55E' :
              (this.proxyDiagnosticState === 'FAIL' ? '#F87171' : '#38BDF8'))
            .width('100%')
          Text(this.proxyEnabled ? this.proxyDiagnosticDetail :
            'Mac 当前为标准转发模式；运行 harmony-netbridge proxy 后重新启动 VPN')
            .fontSize(14)
            .fontColor('#CBD5E1')
            .lineHeight(21)
            .width('100%')
          if (this.proxyEnabled) {
            Text('HTTPS 解密仍需你在系统界面确认信任；应用只下载 Mac 的公共 CA，不会静默安装。')
              .fontSize(12)
              .fontColor('#FBBF24')
              .lineHeight(19)
              .width('100%')
          }
          Button('验证 HTTP 抓包链路')
            .enabled(this.vpnState === 'ACTIVE' && this.proxyEnabled && this.proxyDiagnosticState !== 'RUNNING')
            .backgroundColor('#B45309')
            .fontColor('#FFFFFF')
            .width('100%')
            .onClick(() => {
              this.requestProxyDiagnostics();
            })
          Text(`CA · ${this.certificateState}`)
            .fontSize(13)
            .fontWeight(FontWeight.Medium)
            .fontColor(this.certificateState === 'READY' ? '#22C55E' :
              (this.certificateState === 'FAIL' ? '#F87171' : '#38BDF8'))
            .width('100%')
          Text(this.certificateDetail)
            .fontSize(13)
            .fontColor('#CBD5E1')
            .lineHeight(20)
            .width('100%')
          Button('下载 Mac CA 证书')
            .enabled(this.vpnState === 'ACTIVE' && this.proxyEnabled && this.certificateState !== 'DOWNLOADING')
            .backgroundColor('#0F766E')
            .fontColor('#FFFFFF')
            .width('100%')
            .onClick(() => {
              this.requestCertificateDownload();
            })
          Button('用证书管理器打开')
            .enabled(this.certificateDownloaded && this.certificateState !== 'OPENING')
            .backgroundColor('#334155')
            .fontColor('#FFFFFF')
            .width('100%')
            .onClick(() => {
              this.requestOpenCertificateManager();
            })
        }
        .width('100%')
        .padding(20)
        .backgroundColor('#111C2E')
        .borderRadius(18)

        Column({ space: 12 }) {
          Text('完整 VPN Tunnel')
            .fontSize(18)
            .fontWeight(FontWeight.Bold)
            .fontColor('#F8FAFC')
            .width('100%')
          Text(this.vpnState)
            .fontSize(13)
            .fontWeight(FontWeight.Medium)
            .fontColor(this.vpnState === 'ACTIVE' ? '#22C55E' :
              (this.vpnState === 'FAILED' ? '#F87171' : '#38BDF8'))
            .width('100%')
          Text(this.vpnDetail)
            .fontSize(14)
            .fontColor('#CBD5E1')
            .lineHeight(21)
            .width('100%')
          Row({ space: 12 }) {
            Button('启动 VPN')
              .enabled(this.vpnState !== 'STARTING' && this.vpnState !== 'RECONNECTING' &&
                this.vpnState !== 'ACTIVE' && this.vpnState !== 'STOPPING')
              .backgroundColor('#2563EB')
              .fontColor('#FFFFFF')
              .layoutWeight(1)
              .onClick(() => {
                this.requestVpnStart();
              })
            Button('停止 VPN')
              .enabled(this.vpnState === 'ACTIVE' || this.vpnState === 'STARTING' || this.vpnState === 'RECONNECTING')
              .backgroundColor('#334155')
              .fontColor('#F8FAFC')
              .layoutWeight(1)
              .onClick(() => {
                this.requestVpnStop();
              })
          }
          .width('100%')
          Text('IPv4 默认路由 · MTU 由 Mac 下发 · 双通道心跳 · 自动重连')
            .fontSize(12)
            .fontColor('#94A3B8')
            .width('100%')
        }
        .width('100%')
        .padding(20)
        .backgroundColor('#111C2E')
        .borderRadius(18)

        Column({ space: 12 }) {
          Text('VPN 网络自检')
            .fontSize(18)
            .fontWeight(FontWeight.Bold)
            .fontColor('#F8FAFC')
            .width('100%')
          Text(this.diagnosticState)
            .fontSize(13)
            .fontWeight(FontWeight.Medium)
            .fontColor(this.diagnosticState === 'PASS' ? '#22C55E' :
              (this.diagnosticState === 'FAIL' ? '#F87171' : '#38BDF8'))
            .width('100%')
          Text(this.diagnosticDetail)
            .fontSize(14)
            .fontColor('#CBD5E1')
            .lineHeight(21)
            .width('100%')
          Button('运行 TCP / UDP / DNS 自检')
            .enabled(this.vpnState === 'ACTIVE' && this.diagnosticState !== 'RUNNING')
            .backgroundColor('#0F766E')
            .fontColor('#FFFFFF')
            .width('100%')
            .onClick(() => {
              this.requestNetworkDiagnostics();
            })
        }
        .width('100%')
        .padding(20)
        .backgroundColor('#111C2E')
        .borderRadius(18)

        Column({ space: 12 }) {
          Text('VPN 能力探针')
            .fontSize(18)
            .fontWeight(FontWeight.Bold)
            .fontColor('#F8FAFC')
            .width('100%')
          Text(this.gateStatus)
            .fontSize(13)
            .fontWeight(FontWeight.Medium)
            .fontColor(this.gateStatus === 'DENIED' || this.gateStatus === 'ERROR' ? '#F87171' : '#38BDF8')
            .width('100%')
          Text(this.gateDetail)
            .fontSize(14)
            .fontColor('#CBD5E1')
            .lineHeight(21)
            .width('100%')
          Button('运行 Gate V')
            .enabled(this.gateStatus !== 'STARTING' && this.gateStatus !== 'AUTHORIZED' && this.vpnState !== 'ACTIVE')
            .backgroundColor('#7C3AED')
            .fontColor('#FFFFFF')
            .width('100%')
            .onClick(() => {
              this.requestGateV();
            })
        }
        .width('100%')
        .padding(20)
        .backgroundColor('#111C2E')
        .borderRadius(18)

        Column({ space: 12 }) {
          Text('当前范围')
            .fontSize(18)
            .fontWeight(FontWeight.Bold)
            .fontColor('#F8FAFC')
            .width('100%')

          this.InfoRow('USB 通道', 'hdc rport + loopback TCP')
          this.InfoRow('握手协议', 'HNB/1 hello → world')
          this.InfoRow('VPN', this.vpnState === 'ACTIVE' ? 'IPv4 默认路由已接管' : '当前未接管默认路由')
          this.InfoRow('Relay', 'gVisor TCP / UDP + Mac DNS')
          this.InfoRow('抓包', this.proxyEnabled ? 'mitmweb HTTP(S) TCP' : '未启用')
          this.InfoRow('稳定性', 'Control / Data 心跳 + 退避重连')
        }
        .width('100%')
        .padding(20)
        .backgroundColor('#111C2E')
        .borderRadius(18)

        Text('标准转发运行 harmony-netbridge start；抓包运行 harmony-netbridge proxy。HTTPS CA 必须由用户手动信任。')
          .fontSize(14)
          .fontColor('#94A3B8')
          .lineHeight(21)
          .width('100%')
      }
      .width('100%')
      .padding({ left: 20, right: 20, top: 34, bottom: 34 })
      .backgroundColor('#0B1220')
    }
    .width('100%')
    .height('100%')
    .backgroundColor('#0B1220')
  }

  @Builder
  InfoRow(label: string, value: string) {
    Row({ space: 16 }) {
      Text(label)
        .fontSize(14)
        .fontColor('#94A3B8')
        .width(88)
      Text(value)
        .fontSize(14)
        .fontColor('#E2E8F0')
        .layoutWeight(1)
    }
    .width('100%')
  }
}
