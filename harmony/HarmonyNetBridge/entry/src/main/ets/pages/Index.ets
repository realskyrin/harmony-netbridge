import {
  BridgeConnectionState,
  BridgeSnapshot,
  bridgeSession
} from '../bridge/BridgeSession';

@Entry
@Component
struct Index {
  @State connectionState: BridgeConnectionState = bridgeSession.snapshot.state;
  @State statusTitle: string = bridgeSession.snapshot.title;
  @State statusDetail: string = bridgeSession.snapshot.detail;

  aboutToAppear(): void {
    bridgeSession.setListener((snapshot: BridgeSnapshot) => {
      this.connectionState = snapshot.state;
      this.statusTitle = snapshot.title;
      this.statusDetail = snapshot.detail;
    });
  }

  aboutToDisappear(): void {
    bridgeSession.setListener(undefined);
  }

  private statusColor(): string {
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

  private requestConnect(): void {
    bridgeSession.connect();
  }

  private requestDisconnect(): void {
    bridgeSession.disconnect();
  }

  build() {
    Scroll() {
      Column({ space: 20 }) {
        Column({ space: 8 }) {
          Text('HarmonyNetBridge')
            .fontSize(30)
            .fontWeight(FontWeight.Bold)
            .fontColor('#F8FAFC')
          Text('Phase 1 · USB Control Bridge')
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
            Text(this.connectionState)
              .fontSize(13)
              .fontWeight(FontWeight.Medium)
              .fontColor(this.statusColor())
          }
          .width('100%')

          Text(this.statusTitle)
            .fontSize(22)
            .fontWeight(FontWeight.Bold)
            .fontColor('#F8FAFC')
            .width('100%')

          Text(this.statusDetail)
            .fontSize(15)
            .fontColor('#CBD5E1')
            .lineHeight(23)
            .width('100%')

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
          this.InfoRow('VPN', '已注册骨架，Phase 1 不创建 VPN')
          this.InfoRow('系统流量', '尚未接管（Phase 2）')
        }
        .width('100%')
        .padding(20)
        .backgroundColor('#111C2E')
        .borderRadius(18)

        Text('在 Mac 先运行 harmony-netbridge start，然后保持 USB 调试已授权。')
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
