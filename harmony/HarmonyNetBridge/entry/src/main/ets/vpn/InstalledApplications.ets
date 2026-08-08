import { http } from '@kit.NetworkKit';
import {
  ALWAYS_TUNNELED_BUNDLE_NAME,
  isValidBundleName,
  normalizeBundleName,
  normalizeSelectedApplications
} from './AppWhitelist';

const INSTALLED_APPLICATIONS_URL: string = 'http://127.0.0.1:27183/installed-apps.json';
const TIMEOUT_MS: number = 5000;
const MAX_RESPONSE_CHARACTERS: number = 1024 * 1024;
const MAX_LABEL_LENGTH: number = 256;

export interface InstalledApplication {
  bundleName: string;
  label: string;
}

interface InstalledApplicationsResponse {
  applications: InstalledApplication[];
}

class InstalledApplicationsRequestHeaders {
  Authorization: string;

  constructor(sessionToken: string) {
    this.Authorization = `Bearer ${sessionToken}`;
  }
}

export function decodeInstalledApplications(encoded: string): InstalledApplication[] {
  if (encoded.length === 0 || encoded.length > MAX_RESPONSE_CHARACTERS) {
    return [];
  }
  try {
    const response: InstalledApplicationsResponse = JSON.parse(encoded) as InstalledApplicationsResponse;
    if (response === null || !Array.isArray(response.applications)) {
      return [];
    }
    const applications: InstalledApplication[] = [];
    const seenBundleNames: string[] = [];
    for (let index: number = 0; index < response.applications.length; index++) {
      const candidate: InstalledApplication = response.applications[index];
      if (candidate === undefined || typeof candidate.bundleName !== 'string' ||
        typeof candidate.label !== 'string') {
        continue;
      }
      const bundleName: string = normalizeBundleName(candidate.bundleName);
      const label: string = candidate.label.trim();
      if (!isValidBundleName(bundleName) || bundleName === ALWAYS_TUNNELED_BUNDLE_NAME ||
        label.length > MAX_LABEL_LENGTH ||
        seenBundleNames.indexOf(bundleName) >= 0) {
        continue;
      }
      seenBundleNames.push(bundleName);
      applications.push({ bundleName: bundleName, label: label });
    }
    return applications;
  } catch (error) {
    return [];
  }
}

export function applicationDisplayName(application: InstalledApplication): string {
  if (application.label.length > 0) {
    return application.label;
  }
  const segments: string[] = application.bundleName.split('.');
  return segments[segments.length - 1];
}

export function filterInstalledApplications(applications: InstalledApplication[], query: string): InstalledApplication[] {
  const normalizedQuery: string = query.trim().toLowerCase();
  if (normalizedQuery.length === 0) {
    return applications;
  }
  const filtered: InstalledApplication[] = [];
  for (let index: number = 0; index < applications.length; index++) {
    const application: InstalledApplication = applications[index];
    if (application.bundleName.toLowerCase().indexOf(normalizedQuery) >= 0 ||
      applicationDisplayName(application).toLowerCase().indexOf(normalizedQuery) >= 0) {
      filtered.push(application);
    }
  }
  return filtered;
}

export function prioritizeSelectedApplications(applications: InstalledApplication[],
  selectedBundleNames: string[]): InstalledApplication[] {
  const selectedBundleNamesNormalized: string[] = normalizeSelectedApplications(selectedBundleNames);
  const selected: InstalledApplication[] = [];
  const unselected: InstalledApplication[] = [];
  for (let index: number = 0; index < applications.length; index++) {
    const application: InstalledApplication = applications[index];
    if (selectedBundleNamesNormalized.indexOf(application.bundleName) >= 0) {
      selected.push(application);
    } else {
      unselected.push(application);
    }
  }
  for (let index: number = 0; index < unselected.length; index++) {
    selected.push(unselected[index]);
  }
  return selected;
}

export class InstalledApplicationSource {
  async load(sessionToken: string): Promise<InstalledApplication[]> {
    if (!/^[0-9a-f]{32}$/.test(sessionToken)) {
      throw new Error('Mac 尚未连接，请稍后重试');
    }
    const request: http.HttpRequest = http.createHttp();
    try {
      const response: http.HttpResponse = await request.request(INSTALLED_APPLICATIONS_URL, {
        method: http.RequestMethod.GET,
        header: new InstalledApplicationsRequestHeaders(sessionToken),
        expectDataType: http.HttpDataType.STRING,
        usingCache: false,
        connectTimeout: TIMEOUT_MS,
        readTimeout: TIMEOUT_MS,
        addressFamily: http.AddressFamily.ONLY_V4
      });
      const responseCode: number = response.responseCode as number;
      if (responseCode !== 200 || response.resultType !== http.HttpDataType.STRING) {
        throw new Error(`Mac 返回应用列表失败：HTTP ${responseCode}`);
      }
      const applications: InstalledApplication[] = decodeInstalledApplications(response.result as string);
      if (applications.length === 0) {
        throw new Error('Mac 返回的应用列表为空或格式无效');
      }
      return applications;
    } catch (error) {
      const loadError: Error = error as Error;
      throw new Error(`读取已安装 APP 失败：${loadError.message}`);
    } finally {
      request.destroy();
    }
  }
}
