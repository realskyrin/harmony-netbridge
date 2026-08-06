import { common, Want, wantConstant } from '@kit.AbilityKit';
import { fileIo, fileUri } from '@kit.CoreFileKit';
import { http } from '@kit.NetworkKit';
import { hilog } from '@kit.PerformanceAnalysisKit';

const DOMAIN: number = 0x484E;
const TAG: string = 'HNBCertificate';
const CERTIFICATE_URL: string = 'http://127.0.0.1:27183/mitmproxy-ca-cert.cer';
const CERTIFICATE_FILE_NAME: string = 'mitmproxy-ca-cert.cer';
const CERTIFICATE_UTD: string = 'general.cer-certificate';
const CERTIFICATE_MANAGER_BUNDLE: string = 'com.ohos.certmanager';
const CERTIFICATE_MANAGER_ABILITY: string = 'MainAbility';
const MIN_CERTIFICATE_BYTES: number = 256;
const MAX_CERTIFICATE_BYTES: number = 64 * 1024;
const TIMEOUT_MS: number = 5000;

function startsWithAscii(payload: Uint8Array, marker: string): boolean {
  if (payload.length < marker.length) {
    return false;
  }
  for (let index: number = 0; index < marker.length; index++) {
    if (payload[index] !== marker.charCodeAt(index)) {
      return false;
    }
  }
  return true;
}

// The Mac endpoint performs full X.509/CA validation. This client-side check
// additionally rejects error pages, empty replies, and unexpectedly large data
// before any bytes are persisted on the phone.
export function isCACertificateDownload(responseCode: number, payload: Uint8Array): boolean {
  if (responseCode !== 200 || payload.length < MIN_CERTIFICATE_BYTES ||
    payload.length > MAX_CERTIFICATE_BYTES) {
    return false;
  }
  const isPem: boolean = startsWithAscii(payload, '-----BEGIN CERTIFICATE-----');
  const isDer: boolean = payload[0] === 0x30;
  return isPem || isDer;
}

export class ProxyCertificate {
  private certificatePath(context: common.UIAbilityContext): string {
    return `${context.filesDir}/${CERTIFICATE_FILE_NAME}`;
  }

  hasDownloaded(context: common.UIAbilityContext): boolean {
    try {
      const stat = fileIo.statSync(this.certificatePath(context));
      if (!stat.isFile() || stat.size < MIN_CERTIFICATE_BYTES || stat.size > MAX_CERTIFICATE_BYTES) {
        return false;
      }
      const file = fileIo.openSync(this.certificatePath(context), fileIo.OpenMode.READ_ONLY | fileIo.OpenMode.NOFOLLOW);
      try {
        const buffer: ArrayBuffer = new ArrayBuffer(stat.size);
        const bytesRead: number = fileIo.readSync(file.fd, buffer);
        return bytesRead === stat.size && isCACertificateDownload(200, new Uint8Array(buffer));
      } finally {
        fileIo.closeSync(file);
      }
    } catch (error) {
      return false;
    }
  }

  async download(context: common.UIAbilityContext): Promise<number> {
    const request: http.HttpRequest = http.createHttp();
    try {
      const response: http.HttpResponse = await request.request(CERTIFICATE_URL, {
        method: http.RequestMethod.GET,
        expectDataType: http.HttpDataType.ARRAY_BUFFER,
        usingCache: false,
        connectTimeout: TIMEOUT_MS,
        readTimeout: TIMEOUT_MS,
        addressFamily: http.AddressFamily.ONLY_V4
      });
      const responseCode: number = response.responseCode as number;
      if (response.resultType !== http.HttpDataType.ARRAY_BUFFER) {
        throw new Error('Mac 返回的证书不是二进制数据');
      }
      const payload: Uint8Array = new Uint8Array(response.result as ArrayBuffer);
      if (!isCACertificateDownload(responseCode, payload)) {
        throw new Error(`Mac 返回无效 CA 证书：HTTP ${responseCode}`);
      }
      this.writeCertificate(context, payload);
      hilog.info(DOMAIN, TAG, 'PHASE4_CA_DOWNLOAD_PASS bytes=%{public}d', payload.length);
      return payload.length;
    } catch (error) {
      const downloadError: Error = error as Error;
      throw new Error(`CA 下载失败：${downloadError.message}`);
    } finally {
      request.destroy();
    }
  }

  async openWithCertificateManager(context: common.UIAbilityContext): Promise<void> {
    if (!this.hasDownloaded(context)) {
      throw new Error('请先从 Mac 下载 CA 证书');
    }
    const uri: string = fileUri.getUriFromPath(this.certificatePath(context));
    const want: Want = {
      bundleName: CERTIFICATE_MANAGER_BUNDLE,
      abilityName: CERTIFICATE_MANAGER_ABILITY,
      action: 'ohos.want.action.viewData',
      uri: uri,
      type: CERTIFICATE_UTD,
      flags: wantConstant.Flags.FLAG_AUTH_READ_URI_PERMISSION
    };
    try {
      await context.startAbility(want);
      hilog.info(DOMAIN, TAG, 'PHASE4_CA_CERT_MANAGER_OPENED');
    } catch (error) {
      const openError: Error = error as Error;
      throw new Error(`系统证书管理器启动失败：${openError.message}`);
    }
  }

  private writeCertificate(context: common.UIAbilityContext, payload: Uint8Array): void {
    const mode: number = fileIo.OpenMode.WRITE_ONLY | fileIo.OpenMode.CREATE |
      fileIo.OpenMode.TRUNC | fileIo.OpenMode.NOFOLLOW;
    try {
      const file = fileIo.openSync(this.certificatePath(context), mode);
      try {
        const exactBuffer: ArrayBuffer = payload.slice().buffer as ArrayBuffer;
        const bytesWritten: number = fileIo.writeSync(file.fd, exactBuffer);
        if (bytesWritten !== payload.length) {
          throw new Error(`CA 证书写入不完整：${bytesWritten}/${payload.length}`);
        }
        fileIo.fsyncSync(file.fd);
      } finally {
        fileIo.closeSync(file);
      }
    } catch (error) {
      const writeError: Error = error as Error;
      throw new Error(`CA 证书保存失败：${writeError.message}`);
    }
  }
}
