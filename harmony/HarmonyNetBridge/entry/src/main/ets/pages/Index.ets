import { common, Want } from '@kit.AbilityKit';
import { BusinessError, commonEventManager } from '@kit.BasicServicesKit';
import { vpnExtension } from '@kit.NetworkKit';
import {
  BridgeConnectionState,
  BridgeSnapshot,
  bridgeSession
} from '../bridge/BridgeSession';
import {
  loadTrustedApplications,
  saveTrustedApplications
} from '../vpn/AppWhitelist';
import {
  applicationDisplayName,
  filterInstalledApplications,
  InstalledApplication,
  InstalledApplicationSource
} from '../vpn/InstalledApplications';
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
  @State trustedApplications: string[] = [];
  @State whitelistLoaded: boolean = false;
  @State whitelistSaving: boolean = false;
  @State whitelistDetail: string = '正在读取应用白名单';
  @State whitelistError: string = '';
  @State installedApplications: InstalledApplication[] = [];
  @State applicationSearch: string = '';
  @State applicationListLoaded: boolean = false;
  @State applicationListLoading: boolean = false;
  @State applicationListError: string = '';
  @State showSettings: boolean = false;
  @State developerOptionsError: string = '';
  private proxyCertificate: ProxyCertificate = new ProxyCertificate();
  private installedApplicationSource: InstalledApplicationSource = new InstalledApplicationSource();
  private vpnStatusTimer: number = -1;
  private vpnStatusSubscriber: commonEventManager.CommonEventSubscriber | undefined = undefined;

  aboutToAppear(): void {
    bridgeSession.setListener((snapshot: BridgeSnapshot) => {
      this.connectionState = snapshot.state;
      this.statusTitle = snapshot.title;
      this.statusDetail = snapshot.detail;
      if (snapshot.state === BridgeConnectionState.CONNECTED && this.showSettings &&
        !this.applicationListLoaded && !this.applicationListLoading) {
        this.loadInstalledApplicationList();
      } else if ((snapshot.state === BridgeConnectionState.ERROR ||
        snapshot.state === BridgeConnectionState.DISCONNECTED) && this.showSettings &&
        !this.applicationListLoaded) {
        this.applicationListError = '无法读取已安装 APP：Mac 控制通道未连接';
      }
    });
    this.subscribeVpnStatus();
    this.refreshVpnStatus();
    this.refreshCertificateStatus();
    this.loadAppWhitelist();
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
    if (this.whitelistLoaded && this.trustedApplications.length === 0) {
      return '先选择需要分流的应用';
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
        `白名单中的 ${this.trustedApplications.length} 个应用正通过 USB 转发到 Mac，并启用 HTTP(S) 抓包` :
        `白名单中的 ${this.trustedApplications.length} 个应用正使用 Mac 网络，其余应用保持直连`;
    }
    if (this.vpnState === 'FAILED') {
      return this.vpnDetail;
    }
    if (this.connectionState === BridgeConnectionState.CONNECTED) {
      return this.trustedApplications.length > 0 ?
        '开启 VPN Tunnel 后，仅白名单应用会切换到 Mac 网络' :
        '请先在设置中添加需要通过 Tunnel 的应用';
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
    if (!this.whitelistLoaded || this.trustedApplications.length === 0) {
      this.whitelistError = this.whitelistLoaded ?
        '请至少添加一个应用后再开启 Tunnel' : '应用白名单尚未加载完成，请稍候重试';
      this.showSettings = true;
      return;
    }
    VpnRuntimeState.publish('STARTING', '正在准备 USB 网络通道', false);
    this.refreshVpnStatus();
    try {
      await bridgeSession.disconnect();
      await vpnExtension.startVpnExtensionAbility({
        bundleName: BUNDLE_NAME,
        abilityName: EXTENSION_NAME,
        parameters: {
          mode: 'vpn',
          trustedApplications: JSON.stringify(this.trustedApplications)
        }
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
    if (this.vpnState !== 'ACTIVE' || !this.whitelistContainsBridgeApp() ||
      this.diagnosticState === 'RUNNING') {
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
    if (this.vpnState !== 'ACTIVE' || !this.proxyEnabled || !this.whitelistContainsBridgeApp() ||
      this.proxyDiagnosticState === 'RUNNING') {
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
        return `${this.trustedApplications.length} 个应用 · ${this.proxyEnabled ? '抓包模式' : '标准转发模式'}`;
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
        return this.trustedApplications.length > 0 ?
          `${this.trustedApplications.length} 个应用 · 点击开启` : '请先配置应用白名单';
    }
  }

  private async loadAppWhitelist(): Promise<void> {
    this.whitelistError = '';
    try {
      this.trustedApplications = await loadTrustedApplications(this.uiAbilityContext());
      this.whitelistDetail = this.trustedApplications.length > 0 ?
        `已选择 ${this.trustedApplications.length} 个应用，仅这些应用会经过 VPN` :
        '尚未添加应用；空白名单时 Tunnel 不会启动';
    } catch (error) {
      const loadError: Error = error as Error;
      this.whitelistError = `无法读取应用白名单：${loadError.message}`;
      this.whitelistDetail = '请重新选择应用；Tunnel 会在白名单为空时拒绝启动';
    } finally {
      this.whitelistLoaded = true;
    }
  }

  private whitelistEditable(): boolean {
    return this.whitelistLoaded && !this.whitelistSaving && !this.vpnSwitchOn() && this.vpnState !== 'STOPPING';
  }

  private whitelistContainsBridgeApp(): boolean {
    return this.trustedApplications.indexOf(BUNDLE_NAME) >= 0;
  }

  private whitelistStatusLabel(): string {
    if (!this.whitelistLoaded) {
      return '加载中';
    }
    if (this.whitelistSaving) {
      return '保存中';
    }
    return this.trustedApplications.length === 0 ? '未配置' : `${this.trustedApplications.length} 个应用`;
  }

  private whitelistStatusColor(): string {
    if (!this.whitelistLoaded || this.whitelistSaving) {
      return '#38BDF8';
    }
    return this.trustedApplications.length === 0 ? '#FBBF24' : '#22C55E';
  }

  private async persistAppWhitelist(bundleNames: string[]): Promise<void> {
    if (!this.whitelistEditable()) {
      return;
    }
    this.whitelistSaving = true;
    this.whitelistError = '';
    try {
      this.trustedApplications = await saveTrustedApplications(this.uiAbilityContext(), bundleNames);
      this.whitelistDetail = this.trustedApplications.length > 0 ?
        `已选择 ${this.trustedApplications.length} 个应用，仅这些应用会经过 VPN` :
        '尚未添加应用；空白名单时 Tunnel 不会启动';
    } catch (error) {
      const saveError: Error = error as Error;
      this.whitelistError = `应用白名单保存失败：${saveError.message}`;
    } finally {
      this.whitelistSaving = false;
    }
  }

  private async requestSetWhitelistApp(bundleName: string, selected: boolean): Promise<void> {
    if (!this.whitelistEditable()) {
      return;
    }
    const isSelected: boolean = this.trustedApplications.indexOf(bundleName) >= 0;
    if (selected === isSelected) {
      return;
    }
    const next: string[] = [];
    for (let index: number = 0; index < this.trustedApplications.length; index++) {
      if (this.trustedApplications[index] !== bundleName) {
        next.push(this.trustedApplications[index]);
      }
    }
    if (selected) {
      next.push(bundleName);
    }
    await this.persistAppWhitelist(next);
  }

  private async loadInstalledApplicationList(): Promise<void> {
    if (this.applicationListLoading) {
      return;
    }
    const sessionToken: string = bridgeSession.sessionToken;
    if (sessionToken.length === 0) {
      if (this.vpnOwnsUsbBridge()) {
        this.applicationListError = '请先关闭 Tunnel，再读取或修改已安装 APP 列表';
        return;
      }
      this.applicationListError = '正在连接 Mac，以读取已安装 APP…';
      await bridgeSession.connect();
      return;
    }
    this.applicationListLoading = true;
    this.applicationListError = '';
    try {
      this.installedApplications = await this.installedApplicationSource.load(sessionToken);
      this.applicationListLoaded = true;
    } catch (error) {
      const loadError: Error = error as Error;
      this.applicationListError = `${loadError.message}。请确认 Mac 端桥接已启动后重试`;
      if (this.installedApplications.length === 0) {
        this.applicationListLoaded = false;
      }
    } finally {
      this.applicationListLoading = false;
    }
  }

  private visibleInstalledApplications(): InstalledApplication[] {
    return filterInstalledApplications(this.installedApplications, this.applicationSearch);
  }

  private unavailableTrustedApplications(): string[] {
    if (!this.applicationListLoaded) {
      return [];
    }
    const unavailable: string[] = [];
    for (let trustedIndex: number = 0; trustedIndex < this.trustedApplications.length; trustedIndex++) {
      const bundleName: string = this.trustedApplications[trustedIndex];
      let installed: boolean = false;
      for (let appIndex: number = 0; appIndex < this.installedApplications.length; appIndex++) {
        if (this.installedApplications[appIndex].bundleName === bundleName) {
          installed = true;
          break;
        }
      }
      if (!installed) {
        unavailable.push(bundleName);
      }
    }
    return unavailable;
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
    if (!this.applicationListLoaded && !this.applicationListLoading) {
      this.loadInstalledApplicationList();
    }
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

        Text('应用分流')
          .fontSize(13)
          .fontWeight(FontWeight.Medium)
          .fontColor('#71839C')
          .width('100%')
          .margin({ top: 6 })

        Column({ space: 12 }) {
          this.CardHeader('APP 白名单', this.whitelistStatusLabel(), this.whitelistStatusColor())
          Text('仅白名单中的应用流量经过 VPN，其余应用继续使用设备原网络。')
            .fontSize(14)
            .fontColor('#B8C6D9')
            .lineHeight(21)
            .width('100%')

          if (this.vpnSwitchOn() || this.vpnState === 'STOPPING') {
            Text('请先关闭 Tunnel，再调整应用白名单。')
              .fontSize(13)
              .fontColor('#FBBF24')
              .lineHeight(19)
              .width('100%')
          }

          if (this.whitelistLoaded && this.trustedApplications.length === 0) {
            Text('当前没有应用会进入 VPN；为防止意外接管全部流量，Tunnel 将保持关闭。')
              .fontSize(13)
              .fontColor('#FBBF24')
              .lineHeight(19)
              .width('100%')
          }

          Row({ space: 10 }) {
            TextInput({ placeholder: '搜索 APP 名称或 Bundle 名称', text: this.applicationSearch })
              .height(44)
              .fontSize(13)
              .fontColor('#F8FAFC')
              .placeholderColor('#64748B')
              .backgroundColor('#172337')
              .layoutWeight(1)
              .onChange((value: string) => {
                this.applicationSearch = value;
              })
            Button(this.applicationListLoading ? '读取中' : '刷新')
              .enabled(!this.applicationListLoading)
              .height(44)
              .fontSize(13)
              .fontColor('#FFFFFF')
              .backgroundColor('#2563EB')
              .onClick(() => {
                this.loadInstalledApplicationList();
              })
          }
          .width('100%')

          if (this.applicationListLoading && this.installedApplications.length === 0) {
            Text('正在通过 USB 读取设备上的已安装 APP…')
              .fontSize(13)
              .fontColor('#7DD3FC')
              .width('100%')
          }

          if (this.applicationListLoaded) {
            Text(`${this.visibleInstalledApplications().length} / ${this.installedApplications.length} 个已安装 APP`)
              .fontSize(12)
              .fontColor('#71839C')
              .width('100%')
          }

          ForEach(this.visibleInstalledApplications(), (application: InstalledApplication) => {
            Row({ space: 12 }) {
              Checkbox({ name: application.bundleName, group: 'vpn_app_whitelist' })
                .select(this.trustedApplications.indexOf(application.bundleName) >= 0)
                .enabled(this.whitelistEditable())
                .selectedColor('#2563EB')
                .width(28)
                .height(28)
                .onChange((selected: boolean) => {
                  this.requestSetWhitelistApp(application.bundleName, selected);
                })

              Column({ space: 3 }) {
                Text(applicationDisplayName(application))
                  .fontSize(14)
                  .fontWeight(FontWeight.Medium)
                  .fontColor('#E2E8F0')
                  .maxLines(1)
                  .width('100%')
                Text(application.bundleName === BUNDLE_NAME ?
                  `${application.bundleName} · 用于 Tunnel 网络检测` : application.bundleName)
                  .fontSize(11)
                  .fontColor('#71839C')
                  .maxLines(2)
                  .width('100%')
              }
              .alignItems(HorizontalAlign.Start)
              .layoutWeight(1)
            }
            .width('100%')
            .padding({ left: 12, right: 12, top: 10, bottom: 10 })
            .backgroundColor('#172337')
            .borderRadius(12)
          }, (application: InstalledApplication) => application.bundleName)

          if (this.applicationListLoaded && this.visibleInstalledApplications().length === 0) {
            Text('没有匹配的 APP')
              .fontSize(13)
              .fontColor('#94A3B8')
              .width('100%')
              .padding({ top: 6, bottom: 6 })
          }

          ForEach(this.unavailableTrustedApplications(), (bundleName: string) => {
            Row({ space: 12 }) {
              Column({ space: 3 }) {
                Text(bundleName)
                  .fontSize(13)
                  .fontColor('#CBD5E1')
                  .maxLines(2)
                  .width('100%')
                Text('当前安装列表中已不可用')
                  .fontSize(11)
                  .fontColor('#FBBF24')
                  .width('100%')
              }
              .alignItems(HorizontalAlign.Start)
              .layoutWeight(1)
              Button('移除')
                .enabled(this.whitelistEditable())
                .height(34)
                .fontSize(12)
                .fontColor('#FCA5A5')
                .backgroundColor('#3A2028')
                .onClick(() => {
                  this.requestSetWhitelistApp(bundleName, false);
                })
            }
            .width('100%')
            .padding({ left: 12, right: 8, top: 9, bottom: 9 })
            .backgroundColor('#172337')
            .borderRadius(12)
          }, (bundleName: string) => bundleName)

          Text(this.whitelistDetail)
            .fontSize(12)
            .fontColor('#71839C')
            .lineHeight(18)
            .width('100%')

          if (this.whitelistError.length > 0) {
            Text(this.whitelistError)
              .fontSize(13)
              .fontColor('#FCA5A5')
              .lineHeight(19)
              .width('100%')
          }

          if (this.applicationListError.length > 0) {
            Text(this.applicationListError)
              .fontSize(13)
              .fontColor('#FCA5A5')
              .lineHeight(19)
              .width('100%')
          }
        }
        .width('100%')
        .padding(18)
        .backgroundColor('#111C2E')
        .borderRadius(18)

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
          Text(this.proxyEnabled ?
            (this.whitelistContainsBridgeApp() ? this.proxyDiagnosticDetail :
              `将 ${BUNDLE_NAME} 加入白名单后，才可从本应用检测抓包链路。`) :
            'Mac 当前为标准转发模式。运行 harmony-netbridge proxy 后重新开启 Tunnel，即可使用抓包功能。')
            .fontSize(14)
            .fontColor('#B8C6D9')
            .lineHeight(21)
            .width('100%')
          Button('检测 HTTP 抓包')
            .enabled(this.vpnState === 'ACTIVE' && this.proxyEnabled && this.whitelistContainsBridgeApp() &&
              this.proxyDiagnosticState !== 'RUNNING')
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
          Text(this.whitelistContainsBridgeApp() ? this.diagnosticDetail :
            `将 ${BUNDLE_NAME} 加入白名单后，才可从本应用检测 Tunnel 网络。`)
            .fontSize(14)
            .fontColor('#B8C6D9')
            .lineHeight(21)
            .width('100%')
          Button('运行网络检测')
            .enabled(this.vpnState === 'ACTIVE' && this.whitelistContainsBridgeApp() &&
              this.diagnosticState !== 'RUNNING')
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
          this.InfoRow('流量范围', `${this.trustedApplications.length} 个白名单应用`)
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
