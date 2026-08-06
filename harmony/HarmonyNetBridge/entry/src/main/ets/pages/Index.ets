import { common, Want } from '@kit.AbilityKit';
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
const SETTINGS_BUNDLE_NAME: string = 'com.huawei.hmos.settings';
const SETTINGS_ABILITY_NAME: string = 'com.huawei.hmos.settings.MainAbility';
const DEVELOPER_OPTIONS_URI: string = 'developer_options_settings';

@Entry
@Component
struct Index {
  @State connectionState: BridgeConnectionState = bridgeSession.snapshot.state;
  @State statusTitle: string = bridgeSession.snapshot.title;
  @State statusDetail: string = bridgeSession.snapshot.detail;
  @State gateStatus: string = 'NOT_RUN';
  @State gateDetail: string = '检查当前设备是否支持 VPN 授权与基础网络接管';
  @State vpnState: string = VpnRuntimeState.snapshot.state;
  @State vpnDetail: string = VpnRuntimeState.snapshot.detail;
  @State proxyEnabled: boolean = VpnRuntimeState.snapshot.proxyEnabled;
  @State diagnosticState: string = 'NOT_RUN';
  @State diagnosticDetail: string = '开启 Tunnel 后可检查 TCP、UDP 与 DNS 连接';
  @State proxyDiagnosticState: string = 'NOT_RUN';
  @State proxyDiagnosticDetail: string = '开启抓包模式后可检查 HTTP 流量是否到达 Mac';
  @State certificateState: string = 'NOT_DOWNLOADED';
  @State certificateDetail: string = '证书仅通过 USB 从当前 Mac 下载，不会访问外部证书站点';
  @State certificateDownloaded: boolean = false;
  @State showSettings: boolean = false;
  @State developerOptionsError: string = '';
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
    if (this.vpnState === 'FAILED') {
      return '#F87171';
    }
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
    if (this.vpnOwnsUsbBridge()) {
      return this.vpnStateLabel();
    }
    switch (this.connectionState) {
      case BridgeConnectionState.CONNECTED:
        return 'Mac 已连接';
      case BridgeConnectionState.CONNECTING:
        return '正在连接 Mac';
      case BridgeConnectionState.ERROR:
        return '连接异常';
      case BridgeConnectionState.DISCONNECTED:
        return 'Mac 已断开';
      default:
        return '等待连接';
    }
  }

  private displayedStatusTitle(): string {
    if (this.vpnState === 'ACTIVE') {
      return this.proxyEnabled ? '抓包通道已连接' : '网络通道已连接';
    }
    if (this.vpnState === 'RECONNECTING') {
      return '正在重新连接';
    }
    if (this.vpnState === 'STARTING') {
      return '正在开启 Tunnel';
    }
    if (this.vpnState === 'STOPPING') {
      return '正在关闭 Tunnel';
    }
    if (this.vpnState === 'FAILED') {
      return 'Tunnel 启动失败';
    }
    if (this.vpnOwnsUsbBridge()) {
      return '等待系统确认';
    }
    if (this.connectionState === BridgeConnectionState.CONNECTED) {
      return 'Mac 服务已就绪';
    }
    return '通过 USB 使用 Mac 网络';
  }

  private displayedStatusDetail(): string {
    if (this.vpnState === 'STARTING' || this.vpnState === 'AUTH_REQUIRED') {
      return '请按系统提示完成授权，连接将在确认后自动建立';
    }
    if (this.vpnState === 'RECONNECTING') {
      return 'USB 通道暂时中断，正在自动恢复连接';
    }
    if (this.vpnState === 'STOPPING') {
      return '正在释放网络通道，请稍候';
    }
    if (this.vpnState === 'ACTIVE') {
      return this.proxyEnabled ?
        '设备流量正通过 USB 转发到 Mac，并启用 HTTP(S) 抓包' :
        '设备流量正通过 USB 转发，并使用 Mac 当前的网络连接';
    }
    if (this.vpnState === 'FAILED') {
      return this.vpnDetail;
    }
    if (this.connectionState === BridgeConnectionState.CONNECTED) {
      return '开启 VPN Tunnel 后，设备流量将自动切换到 Mac';
    }
    return '请先在 Mac 启动 HarmonyNetBridge 服务，然后开启 VPN Tunnel';
  }

  private requestConnect(): void {
    bridgeSession.connect();
  }

  private requestDisconnect(): void {
    bridgeSession.disconnect();
  }

  private async requestGateV(): Promise<void> {
    this.gateStatus = 'STARTING';
    this.gateDetail = '正在关闭控制通道并请求系统 VPN 授权';
    try {
      await bridgeSession.disconnect();
      await vpnExtension.startVpnExtensionAbility({
        bundleName: BUNDLE_NAME,
        abilityName: EXTENSION_NAME,
        parameters: { mode: 'gate-v' }
      });
      this.gateStatus = 'REQUESTED';
      this.gateDetail = '请在系统弹窗中确认 VPN 授权';
    } catch (error) {
      const businessError: BusinessError = error as BusinessError;
      this.gateStatus = 'ERROR';
      this.gateDetail = `兼容性检测启动失败：${businessError.code}`;
    }
  }

  private async requestVpnStart(): Promise<void> {
    VpnRuntimeState.publish('STARTING', '正在准备 USB 网络通道', false);
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
    VpnRuntimeState.publish('STOPPING', '正在安全关闭 VPN Tunnel');
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

  private vpnSwitchOn(): boolean {
    return this.vpnState === 'ACTIVE' || this.vpnState === 'STARTING' ||
      this.vpnState === 'RECONNECTING' || this.vpnState === 'AUTH_REQUIRED';
  }

  private vpnToggleEnabled(): boolean {
    return this.vpnState !== 'STOPPING';
  }

  private requestVpnToggle(): void {
    if (this.vpnState === 'ACTIVE' || this.vpnState === 'STARTING' || this.vpnState === 'RECONNECTING') {
      this.requestVpnStop();
      return;
    }
    if (this.vpnState !== 'STOPPING') {
      this.requestVpnStart();
    }
  }

  private vpnStateLabel(): string {
    switch (this.vpnState) {
      case 'ACTIVE':
        return 'Tunnel 已开启';
      case 'STARTING':
        return 'Tunnel 启动中';
      case 'AUTH_REQUIRED':
        return '等待 VPN 授权';
      case 'RECONNECTING':
        return 'Tunnel 重连中';
      case 'STOPPING':
        return 'Tunnel 正在关闭';
      case 'FAILED':
        return 'Tunnel 启动失败';
      default:
        return 'Tunnel 已关闭';
    }
  }

  private vpnToggleDetail(): string {
    switch (this.vpnState) {
      case 'ACTIVE':
        return this.proxyEnabled ? '抓包模式' : '标准转发模式';
      case 'STARTING':
      case 'AUTH_REQUIRED':
        return '正在建立安全连接';
      case 'RECONNECTING':
        return '正在自动恢复连接';
      case 'STOPPING':
        return '正在释放网络资源';
      case 'FAILED':
        return '点击重试';
      default:
        return '点击开启';
    }
  }

  private vpnToggleBackground(): string {
    if (this.vpnState === 'ACTIVE') {
      return '#0F8A6A';
    }
    if (this.vpnSwitchOn() || this.vpnState === 'STOPPING') {
      return '#2563EB';
    }
    if (this.vpnState === 'FAILED') {
      return '#7F1D1D';
    }
    return '#1E293B';
  }

  private certificateStatusLabel(): string {
    switch (this.certificateState) {
      case 'READY':
        return '已下载';
      case 'DOWNLOADING':
        return '下载中';
      case 'OPENING':
        return '正在打开';
      case 'FAIL':
        return '操作失败';
      default:
        return '未下载';
    }
  }

  private certificateStatusColor(): string {
    if (this.certificateState === 'READY') {
      return '#22C55E';
    }
    if (this.certificateState === 'FAIL') {
      return '#F87171';
    }
    return '#38BDF8';
  }

  private certificateHomeDetail(): string {
    if (this.certificateDownloaded) {
      return '证书已保存到本机，安装与信任需要在系统界面中确认';
    }
    if (this.vpnState !== 'ACTIVE') {
      return '开启抓包模式的 Tunnel 后，即可从 Mac 安全下载 CA 证书';
    }
    if (!this.proxyEnabled) {
      return 'Mac 当前为标准转发模式，请使用 proxy 模式后重新开启 Tunnel';
    }
    return this.certificateDetail;
  }

  private certificateActionTitle(): string {
    if (this.certificateState === 'DOWNLOADING') {
      return '正在下载证书…';
    }
    if (this.certificateState === 'OPENING') {
      return '正在打开证书管理器…';
    }
    return this.certificateDownloaded ? '打开证书管理器' : '下载 CA 证书';
  }

  private certificateActionEnabled(): boolean {
    if (this.certificateDownloaded) {
      return this.certificateState !== 'OPENING';
    }
    return this.vpnState === 'ACTIVE' && this.proxyEnabled && this.certificateState !== 'DOWNLOADING';
  }

  private requestCertificatePrimaryAction(): void {
    if (this.certificateDownloaded) {
      this.requestOpenCertificateManager();
      return;
    }
    this.requestCertificateDownload();
  }

  private diagnosticStateLabel(state: string): string {
    switch (state) {
      case 'RUNNING':
      case 'STARTING':
        return '检测中';
      case 'PASS':
      case 'AUTHORIZED':
        return '正常';
      case 'FAIL':
      case 'ERROR':
        return '检测失败';
      case 'DENIED':
        return '未授权';
      case 'REQUESTED':
        return '等待系统确认';
      default:
        return '未检测';
    }
  }

  private diagnosticStateColor(state: string): string {
    if (state === 'PASS' || state === 'AUTHORIZED') {
      return '#22C55E';
    }
    if (state === 'FAIL' || state === 'ERROR' || state === 'DENIED') {
      return '#F87171';
    }
    return '#38BDF8';
  }

  private async requestOpenDeveloperOptions(): Promise<void> {
    this.developerOptionsError = '';
    const want: Want = {
      bundleName: SETTINGS_BUNDLE_NAME,
      abilityName: SETTINGS_ABILITY_NAME,
      uri: DEVELOPER_OPTIONS_URI
    };
    try {
      await this.uiAbilityContext().startAbility(want);
    } catch (error) {
      const businessError: BusinessError = error as BusinessError;
      this.developerOptionsError = businessError.code === undefined ?
        '无法打开开发者选项，请从系统设置中手动进入' :
        `无法打开开发者选项（错误 ${businessError.code}）`;
    }
  }

  private openSettings(): void {
    this.showSettings = true;
  }

  private closeSettings(): void {
    this.showSettings = false;
  }

  onBackPress(): boolean {
    if (this.showSettings) {
      this.closeSettings();
      return true;
    }
    return false;
  }

  build() {
    Stack() {
      if (this.showSettings) {
        this.SettingsPage()
      } else {
        this.HomePage()
      }
    }
    .width('100%')
    .height('100%')
    .backgroundColor('#09111F')
  }

  @Builder
  HomePage() {
    Scroll() {
      Column({ space: 18 }) {
        Row({ space: 16 }) {
          Column({ space: 4 }) {
            Text('HarmonyNetBridge')
              .fontSize(23)
              .fontWeight(FontWeight.Bold)
              .fontColor('#F8FAFC')
              .maxLines(1)
            Text('USB 网络桥接')
              .fontSize(14)
              .fontColor('#8FA2BD')
          }
          .alignItems(HorizontalAlign.Start)
          .layoutWeight(1)

          Button('设置')
            .height(40)
            .padding({ left: 18, right: 18 })
            .fontSize(14)
            .fontColor('#DCE8F8')
            .backgroundColor('#172337')
            .border({ width: 1, color: '#26374F' })
            .onClick(() => {
              this.openSettings();
            })
        }
        .width('100%')

        Column({ space: 16 }) {
          Row({ space: 9 }) {
            Text('')
              .width(9)
              .height(9)
              .backgroundColor(this.statusColor())
              .borderRadius(5)
            Text(this.displayedConnectionState())
              .fontSize(13)
              .fontWeight(FontWeight.Medium)
              .fontColor(this.statusColor())
          }
          .width('100%')

          Column({ space: 7 }) {
            Text(this.displayedStatusTitle())
              .fontSize(24)
              .fontWeight(FontWeight.Bold)
              .fontColor('#F8FAFC')
              .width('100%')
            Text(this.displayedStatusDetail())
              .fontSize(14)
              .fontColor('#B8C6D9')
              .lineHeight(21)
              .width('100%')
          }
          .alignItems(HorizontalAlign.Start)
          .width('100%')

          Button() {
            Row({ space: 16 }) {
              Column({ space: 4 }) {
                Text('VPN Tunnel')
                  .fontSize(17)
                  .fontWeight(FontWeight.Bold)
                  .fontColor('#FFFFFF')
                Text(this.vpnToggleDetail())
                  .fontSize(13)
                  .fontColor('#E2E8F0')
              }
              .alignItems(HorizontalAlign.Start)
              .layoutWeight(1)

              Stack() {
                Text('')
                  .width(22)
                  .height(22)
                  .backgroundColor('#FFFFFF')
                  .borderRadius(11)
                  .translate({ x: this.vpnSwitchOn() ? 11 : -11 })
                  .animation({ duration: 280, curve: Curve.EaseInOut })
              }
              .width(52)
              .height(30)
              .borderRadius(15)
              .backgroundColor(this.vpnSwitchOn() ? '#52FFFFFF' : '#24FFFFFF')
              .animation({ duration: 280, curve: Curve.EaseInOut })
            }
            .width('100%')
          }
          .enabled(this.vpnToggleEnabled())
          .height(68)
          .padding({ left: 18, right: 18 })
          .backgroundColor(this.vpnToggleBackground())
          .animation({ duration: 280, curve: Curve.EaseInOut })
          .onClick(() => {
            this.requestVpnToggle();
          })
        }
        .width('100%')
        .padding(20)
        .backgroundColor('#111C2E')
        .borderRadius(20)
        .border({ width: 1, color: '#1E3048' })

        Column({ space: 13 }) {
          Row({ space: 12 }) {
            Text('CA')
              .fontSize(12)
              .fontWeight(FontWeight.Bold)
              .fontColor('#DFF7F0')
              .width(36)
              .height(36)
              .textAlign(TextAlign.Center)
              .backgroundColor('#0F766E')
              .borderRadius(10)
            Column({ space: 3 }) {
              Text('HTTPS 调试证书')
                .fontSize(17)
                .fontWeight(FontWeight.Bold)
                .fontColor('#F8FAFC')
              Text(this.certificateStatusLabel())
                .fontSize(12)
                .fontWeight(FontWeight.Medium)
                .fontColor(this.certificateStatusColor())
            }
            .alignItems(HorizontalAlign.Start)
            .layoutWeight(1)
          }
          .width('100%')

          Text(this.certificateHomeDetail())
            .fontSize(14)
            .fontColor('#B8C6D9')
            .lineHeight(21)
            .width('100%')

          Button(this.certificateActionTitle())
            .enabled(this.certificateActionEnabled())
            .height(48)
            .fontSize(15)
            .fontWeight(FontWeight.Medium)
            .fontColor('#FFFFFF')
            .backgroundColor('#0F766E')
            .width('100%')
            .onClick(() => {
              this.requestCertificatePrimaryAction();
            })
        }
        .width('100%')
        .padding(20)
        .backgroundColor('#111C2E')
        .borderRadius(20)
        .border({ width: 1, color: '#1E3048' })

        Button() {
          Row({ space: 14 }) {
            Text('</>')
              .fontSize(14)
              .fontWeight(FontWeight.Bold)
              .fontColor('#A5B4FC')
              .width(38)
              .height(38)
              .textAlign(TextAlign.Center)
              .backgroundColor('#252B50')
              .borderRadius(11)
            Column({ space: 3 }) {
              Text('开发者选项')
                .fontSize(16)
                .fontWeight(FontWeight.Bold)
                .fontColor('#F8FAFC')
              Text('打开系统设置并管理 USB 调试')
                .fontSize(13)
                .fontColor('#94A3B8')
            }
            .alignItems(HorizontalAlign.Start)
            .layoutWeight(1)
            Text('›')
              .fontSize(26)
              .fontColor('#8FA2BD')
          }
          .width('100%')
        }
        .height(72)
        .padding({ left: 16, right: 16 })
        .backgroundColor('#111C2E')
        .border({ width: 1, color: '#1E3048' })
        .onClick(() => {
          this.requestOpenDeveloperOptions();
        })

        if (this.developerOptionsError.length > 0) {
          Text(this.developerOptionsError)
            .fontSize(13)
            .fontColor('#FBBF24')
            .lineHeight(19)
            .width('100%')
        }

        Text('需要先在 Mac 端启动 HarmonyNetBridge 服务')
          .fontSize(12)
          .fontColor('#64748B')
          .textAlign(TextAlign.Center)
          .width('100%')
      }
      .width('100%')
      .padding({ left: 20, right: 20, top: 34, bottom: 30 })
    }
    .width('100%')
    .height('100%')
    .backgroundColor('#09111F')
    .scrollBar(BarState.Off)
  }

  @Builder
  SettingsPage() {
    Scroll() {
      Column({ space: 16 }) {
        Row({ space: 12 }) {
          Button('‹')
            .width(42)
            .height(42)
            .fontSize(28)
            .fontColor('#F8FAFC')
            .backgroundColor('#172337')
            .border({ width: 1, color: '#26374F' })
            .onClick(() => {
              this.closeSettings();
            })
          Text('设置')
            .fontSize(27)
            .fontWeight(FontWeight.Bold)
            .fontColor('#F8FAFC')
            .layoutWeight(1)
        }
        .width('100%')

        Text('连接与诊断')
          .fontSize(13)
          .fontWeight(FontWeight.Medium)
          .fontColor('#71839C')
          .width('100%')
          .margin({ top: 6 })

        Column({ space: 12 }) {
          this.CardHeader('Mac 控制通道', this.displayedConnectionState(), this.statusColor())
          Text(this.vpnOwnsUsbBridge() ?
            'VPN Tunnel 正在管理 USB 连接，关闭 Tunnel 后可单独测试控制通道。' : this.statusDetail)
            .fontSize(14)
            .fontColor('#B8C6D9')
            .lineHeight(21)
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
              Button('断开连接')
                .enabled(this.connectionState === BridgeConnectionState.CONNECTED ||
                  this.connectionState === BridgeConnectionState.CONNECTING)
                .backgroundColor('#26364D')
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
        .padding(18)
        .backgroundColor('#111C2E')
        .borderRadius(18)

        Column({ space: 12 }) {
          this.CardHeader('抓包链路', this.proxyEnabled ?
            this.diagnosticStateLabel(this.proxyDiagnosticState) : '未启用',
            this.proxyEnabled ? this.diagnosticStateColor(this.proxyDiagnosticState) : '#94A3B8')
          Text(this.proxyEnabled ? this.proxyDiagnosticDetail :
            'Mac 当前为标准转发模式。运行 harmony-netbridge proxy 后重新开启 Tunnel，即可使用抓包功能。')
            .fontSize(14)
            .fontColor('#B8C6D9')
            .lineHeight(21)
            .width('100%')
          Button('检测 HTTP 抓包')
            .enabled(this.vpnState === 'ACTIVE' && this.proxyEnabled && this.proxyDiagnosticState !== 'RUNNING')
            .backgroundColor('#B45309')
            .fontColor('#FFFFFF')
            .width('100%')
            .onClick(() => {
              this.requestProxyDiagnostics();
            })
        }
        .width('100%')
        .padding(18)
        .backgroundColor('#111C2E')
        .borderRadius(18)

        Column({ space: 12 }) {
          this.CardHeader('网络连接检测', this.diagnosticStateLabel(this.diagnosticState),
            this.diagnosticStateColor(this.diagnosticState))
          Text(this.diagnosticDetail)
            .fontSize(14)
            .fontColor('#B8C6D9')
            .lineHeight(21)
            .width('100%')
          Button('运行网络检测')
            .enabled(this.vpnState === 'ACTIVE' && this.diagnosticState !== 'RUNNING')
            .backgroundColor('#0F766E')
            .fontColor('#FFFFFF')
            .width('100%')
            .onClick(() => {
              this.requestNetworkDiagnostics();
            })
        }
        .width('100%')
        .padding(18)
        .backgroundColor('#111C2E')
        .borderRadius(18)

        Text('高级选项')
          .fontSize(13)
          .fontWeight(FontWeight.Medium)
          .fontColor('#71839C')
          .width('100%')
          .margin({ top: 6 })

        Column({ space: 12 }) {
          this.CardHeader('证书管理', this.certificateStatusLabel(), this.certificateStatusColor())
          Text(this.certificateDetail)
            .fontSize(14)
            .fontColor('#B8C6D9')
            .lineHeight(21)
            .width('100%')
          Row({ space: 12 }) {
            Button('重新下载')
              .enabled(this.vpnState === 'ACTIVE' && this.proxyEnabled && this.certificateState !== 'DOWNLOADING')
              .backgroundColor('#0F766E')
              .fontColor('#FFFFFF')
              .layoutWeight(1)
              .onClick(() => {
                this.requestCertificateDownload();
              })
            Button('系统证书管理')
              .enabled(this.certificateDownloaded && this.certificateState !== 'OPENING')
              .backgroundColor('#26364D')
              .fontColor('#FFFFFF')
              .layoutWeight(1)
              .onClick(() => {
                this.requestOpenCertificateManager();
              })
          }
          .width('100%')
        }
        .width('100%')
        .padding(18)
        .backgroundColor('#111C2E')
        .borderRadius(18)

        Column({ space: 12 }) {
          this.CardHeader('VPN 兼容性检测', this.diagnosticStateLabel(this.gateStatus),
            this.diagnosticStateColor(this.gateStatus))
          Text(this.gateDetail)
            .fontSize(14)
            .fontColor('#B8C6D9')
            .lineHeight(21)
            .width('100%')
          Button('开始兼容性检测')
            .enabled(this.gateStatus !== 'STARTING' && this.gateStatus !== 'AUTHORIZED' &&
              this.vpnState !== 'ACTIVE')
            .backgroundColor('#6D4BD1')
            .fontColor('#FFFFFF')
            .width('100%')
            .onClick(() => {
              this.requestGateV();
            })
        }
        .width('100%')
        .padding(18)
        .backgroundColor('#111C2E')
        .borderRadius(18)

        Column({ space: 12 }) {
          Text('技术信息')
            .fontSize(17)
            .fontWeight(FontWeight.Bold)
            .fontColor('#F8FAFC')
            .width('100%')
          this.InfoRow('USB 通道', 'hdc rport + 本机回环连接')
          this.InfoRow('网络协议', 'IPv4 TCP / UDP / DNS')
          this.InfoRow('VPN 状态', this.vpnStateLabel())
          this.InfoRow('流量转发', 'gVisor 用户态网络栈')
          this.InfoRow('抓包模式', this.proxyEnabled ? 'mitmweb 已启用' : '未启用')
          this.InfoRow('连接恢复', '心跳检测与自动重连')
        }
        .width('100%')
        .padding(18)
        .backgroundColor('#111C2E')
        .borderRadius(18)

        Text('标准转发请在 Mac 运行 harmony-netbridge start；抓包请运行 harmony-netbridge proxy。HTTPS 证书必须由用户在系统界面中确认安装与信任。')
          .fontSize(13)
          .fontColor('#71839C')
          .lineHeight(20)
          .width('100%')
      }
      .width('100%')
      .padding({ left: 20, right: 20, top: 30, bottom: 34 })
    }
    .width('100%')
    .height('100%')
    .backgroundColor('#09111F')
    .scrollBar(BarState.Off)
  }

  @Builder
  CardHeader(title: string, state: string, color: string) {
    Row({ space: 12 }) {
      Text(title)
        .fontSize(17)
        .fontWeight(FontWeight.Bold)
        .fontColor('#F8FAFC')
        .layoutWeight(1)
      Text(state)
        .fontSize(12)
        .fontWeight(FontWeight.Medium)
        .fontColor(color)
        .padding({ left: 10, right: 10, top: 5, bottom: 5 })
        .backgroundColor('#19263A')
        .borderRadius(12)
    }
    .width('100%')
  }

  @Builder
  InfoRow(label: string, value: string) {
    Row({ space: 16 }) {
      Text(label)
        .fontSize(13)
        .fontColor('#8091A9')
        .width(82)
      Text(value)
        .fontSize(13)
        .fontColor('#DCE6F3')
        .layoutWeight(1)
    }
    .width('100%')
  }
}
