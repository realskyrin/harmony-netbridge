import { common } from '@kit.AbilityKit';
import { preferences } from '@kit.ArkData';
import { vpnExtension } from '@kit.NetworkKit';

// Keep the original preferences namespace and selection key so existing users
// retain their user-managed selections. The host App is migrated out because
// it is now an implicit member of every VPN routing configuration.
const PREFERENCES_NAME: string = 'vpn_app_whitelist';
const SELECTED_APPLICATIONS_KEY: string = 'trusted_applications';
const ROUTING_MODE_KEY: string = 'routing_mode';
const MIN_BUNDLE_NAME_LENGTH: number = 7;
const MAX_BUNDLE_NAME_LENGTH: number = 128;
export const ALWAYS_TUNNELED_BUNDLE_NAME: string = 'io.github.realskyrin.harmonynetbridge';
export const VPN_APPLICATION_ISOLATION_MIN_API_VERSION: number = 20;

export enum AppRoutingMode {
  WHITELIST = 'whitelist',
  BLACKLIST = 'blacklist'
}

export class AppRoutingPreferences {
  readonly mode: AppRoutingMode;
  readonly selectedApplications: string[];

  constructor(mode: AppRoutingMode, selectedApplications: string[]) {
    this.mode = mode;
    this.selectedApplications = selectedApplications;
  }
}

export class VpnApplicationRouting {
  readonly trustedApplications: string[] | undefined;
  readonly blockedApplications: string[] | undefined;

  constructor(trustedApplications: string[] | undefined, blockedApplications: string[] | undefined) {
    this.trustedApplications = trustedApplications;
    this.blockedApplications = blockedApplications;
  }
}

interface VpnConfigWithIdentity extends vpnExtension.VpnConfig {
  vpnId?: string;
}

export function supportsVpnApplicationIsolation(sdkApiVersion: number): boolean {
  return sdkApiVersion >= VPN_APPLICATION_ISOLATION_MIN_API_VERSION;
}

export function usesIsolatedVpnApplicationRouting(mode: AppRoutingMode): boolean {
  return mode === AppRoutingMode.WHITELIST;
}

export function supportsVpnApplicationRouting(mode: AppRoutingMode, sdkApiVersion: number): boolean {
  return !usesIsolatedVpnApplicationRouting(mode) || supportsVpnApplicationIsolation(sdkApiVersion);
}

export function vpnApplicationRouteInterface(mode: AppRoutingMode): string {
  return usesIsolatedVpnApplicationRouting(mode) ? '' : 'vpn-tun';
}

function isAsciiLetter(code: number): boolean {
  return (code >= 65 && code <= 90) || (code >= 97 && code <= 122);
}

function isAsciiDigit(code: number): boolean {
  return code >= 48 && code <= 57;
}

function isBundleSegmentCharacter(code: number): boolean {
  return isAsciiLetter(code) || isAsciiDigit(code) || code === 95;
}

// Bundle names come from the Mac-provided installed-App list, but are validated
// again at persistence and VPN start boundaries so malformed input fails closed.
export function normalizeBundleName(bundleName: string): string {
  return bundleName.trim();
}

export function isValidBundleName(bundleName: string): boolean {
  const normalized: string = normalizeBundleName(bundleName);
  if (normalized.length < MIN_BUNDLE_NAME_LENGTH || normalized.length > MAX_BUNDLE_NAME_LENGTH) {
    return false;
  }
  const segments: string[] = normalized.split('.');
  if (segments.length < 3) {
    return false;
  }
  for (let segmentIndex: number = 0; segmentIndex < segments.length; segmentIndex++) {
    const segment: string = segments[segmentIndex];
    if (segment.length === 0) {
      return false;
    }
    const firstCode: number = segment.charCodeAt(0);
    const lastCode: number = segment.charCodeAt(segment.length - 1);
    if (segmentIndex === 0) {
      if (!isAsciiLetter(firstCode)) {
        return false;
      }
    } else if (!isAsciiLetter(firstCode) && !isAsciiDigit(firstCode)) {
      return false;
    }
    if (!isAsciiLetter(lastCode) && !isAsciiDigit(lastCode)) {
      return false;
    }
    for (let characterIndex: number = 0; characterIndex < segment.length; characterIndex++) {
      if (!isBundleSegmentCharacter(segment.charCodeAt(characterIndex))) {
        return false;
      }
    }
  }
  return true;
}

export function normalizeSelectedApplications(bundleNames: string[]): string[] {
  const normalized: string[] = [];
  for (let index: number = 0; index < bundleNames.length; index++) {
    if (typeof bundleNames[index] !== 'string') {
      continue;
    }
    const bundleName: string = normalizeBundleName(bundleNames[index]);
    if (isValidBundleName(bundleName) && bundleName !== ALWAYS_TUNNELED_BUNDLE_NAME &&
      normalized.indexOf(bundleName) < 0) {
      normalized.push(bundleName);
    }
  }
  return normalized;
}

export function decodeSelectedApplications(encoded: string): string[] {
  try {
    const decoded: string[] = JSON.parse(encoded) as string[];
    if (!Array.isArray(decoded)) {
      return [];
    }
    return normalizeSelectedApplications(decoded);
  } catch (error) {
    return [];
  }
}

export function decodeAppRoutingMode(encoded: string): AppRoutingMode {
  return encoded === AppRoutingMode.BLACKLIST ? AppRoutingMode.BLACKLIST : AppRoutingMode.WHITELIST;
}

export function applicationUsesTunnel(mode: AppRoutingMode, selectedApplications: string[],
  bundleName: string): boolean {
  const normalizedBundleName: string = normalizeBundleName(bundleName);
  if (normalizedBundleName === ALWAYS_TUNNELED_BUNDLE_NAME) {
    return true;
  }
  const selected: boolean = normalizeSelectedApplications(selectedApplications).indexOf(normalizedBundleName) >= 0;
  return mode === AppRoutingMode.WHITELIST ? selected : !selected;
}

export function buildVpnApplicationRouting(mode: AppRoutingMode,
  selectedApplications: string[]): VpnApplicationRouting {
  const normalized: string[] = normalizeSelectedApplications(selectedApplications);
  if (mode === AppRoutingMode.BLACKLIST) {
    return new VpnApplicationRouting(undefined, normalized.length > 0 ? normalized : undefined);
  }
  const trustedApplications: string[] = [ALWAYS_TUNNELED_BUNDLE_NAME];
  for (let index: number = 0; index < normalized.length; index++) {
    trustedApplications.push(normalized[index]);
  }
  return new VpnApplicationRouting(trustedApplications, undefined);
}

export function applyVpnApplicationRouting(config: vpnExtension.VpnConfig,
  routing: VpnApplicationRouting): void {
  if (routing.trustedApplications !== undefined) {
    config.trustedApplications = routing.trustedApplications;
  } else if (routing.blockedApplications !== undefined) {
    config.blockedApplications = routing.blockedApplications;
  }
}

// Whitelist routing supplies a generated vpnId to select HarmonyOS' isolated
// multi-VPN path. Unlike the legacy global vpn-tun path, that path marks only
// the configured App UID ranges. Leave route interfaces empty so the system
// can bind them to the generated multitun-vpnN interface.
export function applyVpnApplicationIsolation(config: vpnExtension.VpnConfig, vpnId: string): void {
  const normalizedVpnId: string = vpnId.trim();
  if (normalizedVpnId.length === 0) {
    throw new Error('generated VPN ID is empty');
  }
  const isolatedConfig: VpnConfigWithIdentity = config as VpnConfigWithIdentity;
  isolatedConfig.vpnId = normalizedVpnId;
  const routes: vpnExtension.RouteInfo[] | undefined = config.routes;
  if (routes === undefined) {
    return;
  }
  for (let index: number = 0; index < routes.length; index++) {
    routes[index].interface = '';
  }
}

export function canStartAppRouting(mode: AppRoutingMode, selectedApplications: string[]): boolean {
  if (mode === AppRoutingMode.BLACKLIST) {
    return true;
  }
  const trustedApplications: string[] | undefined =
    buildVpnApplicationRouting(mode, selectedApplications).trustedApplications;
  return trustedApplications !== undefined && trustedApplications.length > 0;
}

function sameApplicationSelection(stored: string[], normalized: string[]): boolean {
  if (stored.length !== normalized.length) {
    return false;
  }
  for (let index: number = 0; index < stored.length; index++) {
    if (typeof stored[index] !== 'string' || stored[index] !== normalized[index]) {
      return false;
    }
  }
  return true;
}

export async function loadAppRoutingPreferences(context: common.UIAbilityContext): Promise<AppRoutingPreferences> {
  try {
    const store: preferences.Preferences = await preferences.getPreferences(context, PREFERENCES_NAME);
    const storedApplications: preferences.ValueType = await store.get(SELECTED_APPLICATIONS_KEY, []);
    const storedMode: preferences.ValueType = await store.get(ROUTING_MODE_KEY, AppRoutingMode.WHITELIST);
    const storedSelection: string[] = Array.isArray(storedApplications) ? storedApplications as string[] : [];
    const selectedApplications: string[] = normalizeSelectedApplications(storedSelection);
    if (!sameApplicationSelection(storedSelection, selectedApplications)) {
      await store.put(SELECTED_APPLICATIONS_KEY, selectedApplications);
      await store.flush();
    }
    return new AppRoutingPreferences(decodeAppRoutingMode(storedMode as string), selectedApplications);
  } catch (error) {
    const preferencesError: Error = error as Error;
    throw new Error(`read app routing preferences failed: ${preferencesError.message}`);
  }
}

export async function saveSelectedApplications(context: common.UIAbilityContext,
  bundleNames: string[]): Promise<string[]> {
  const normalized: string[] = normalizeSelectedApplications(bundleNames);
  try {
    const store: preferences.Preferences = await preferences.getPreferences(context, PREFERENCES_NAME);
    await store.put(SELECTED_APPLICATIONS_KEY, normalized);
    await store.flush();
    return normalized;
  } catch (error) {
    const preferencesError: Error = error as Error;
    throw new Error(`write app routing preferences failed: ${preferencesError.message}`);
  }
}

export async function saveAppRoutingMode(context: common.UIAbilityContext,
  mode: AppRoutingMode): Promise<AppRoutingMode> {
  const normalized: AppRoutingMode = decodeAppRoutingMode(mode);
  try {
    const store: preferences.Preferences = await preferences.getPreferences(context, PREFERENCES_NAME);
    await store.put(ROUTING_MODE_KEY, normalized);
    await store.flush();
    return normalized;
  } catch (error) {
    const preferencesError: Error = error as Error;
    throw new Error(`write app routing mode failed: ${preferencesError.message}`);
  }
}
