import { common } from '@kit.AbilityKit';
import { preferences } from '@kit.ArkData';

const PREFERENCES_NAME: string = 'vpn_app_whitelist';
const TRUSTED_APPLICATIONS_KEY: string = 'trusted_applications';
const MIN_BUNDLE_NAME_LENGTH: number = 7;
const MAX_BUNDLE_NAME_LENGTH: number = 128;

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

export function normalizeTrustedApplications(bundleNames: string[]): string[] {
  const normalized: string[] = [];
  for (let index: number = 0; index < bundleNames.length; index++) {
    const bundleName: string = normalizeBundleName(bundleNames[index]);
    if (isValidBundleName(bundleName) && normalized.indexOf(bundleName) < 0) {
      normalized.push(bundleName);
    }
  }
  return normalized;
}

export function decodeTrustedApplications(encoded: string): string[] {
  try {
    const decoded: string[] = JSON.parse(encoded) as string[];
    if (!Array.isArray(decoded)) {
      return [];
    }
    return normalizeTrustedApplications(decoded);
  } catch (error) {
    return [];
  }
}

export async function loadTrustedApplications(context: common.UIAbilityContext): Promise<string[]> {
  try {
    const store: preferences.Preferences = await preferences.getPreferences(context, PREFERENCES_NAME);
    const stored: preferences.ValueType = await store.get(TRUSTED_APPLICATIONS_KEY, []);
    return normalizeTrustedApplications(stored as string[]);
  } catch (error) {
    const preferencesError: Error = error as Error;
    throw new Error(`read whitelist preferences failed: ${preferencesError.message}`);
  }
}

export async function saveTrustedApplications(context: common.UIAbilityContext,
  bundleNames: string[]): Promise<string[]> {
  const normalized: string[] = normalizeTrustedApplications(bundleNames);
  try {
    const store: preferences.Preferences = await preferences.getPreferences(context, PREFERENCES_NAME);
    await store.put(TRUSTED_APPLICATIONS_KEY, normalized);
    await store.flush();
    return normalized;
  } catch (error) {
    const preferencesError: Error = error as Error;
    throw new Error(`write whitelist preferences failed: ${preferencesError.message}`);
  }
}
