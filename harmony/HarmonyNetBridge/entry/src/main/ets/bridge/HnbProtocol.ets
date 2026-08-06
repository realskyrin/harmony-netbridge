export const HNB_HEADER_SIZE: number = 16;
export const HNB_VERSION: number = 1;
export const HNB_MAX_CONTROL_PAYLOAD: number = 16 * 1024;
export const HNB_MAX_PACKET_PAYLOAD: number = 65535;
export const HNB_MIN_MTU: number = 576;
export const HNB_MAX_MTU: number = 1500;

export enum HnbFrameType {
  HELLO = 0x01,
  HELLO_ACK = 0x02,
  ERROR = 0x03,
  STOP_REQUEST = 0x04,
  STOP_ACK = 0x05,
  DATA_HELLO = 0x10,
  DATA_ACK = 0x11,
  VPN_STATUS = 0x12,
  IP_PACKET = 0x20,
  PING = 0x30,
  PONG = 0x31
}

export interface HelloPayload {
  role: string;
  mode: string;
  appVersion: string;
  supportedVersions: number[];
  capabilities: string[];
  message: string;
}

export interface HelloAckPayload {
  selectedVersion: number;
  sessionToken: string;
  capabilities: string[];
  mtu: number;
  message: string;
}

export interface ErrorPayload {
  code: string;
  message: string;
  fatal: boolean;
}

export interface StopRequestPayload {
  reason: string;
}

export interface DataHelloPayload {
  sessionToken: string;
  role: string;
}

export interface VpnStatusPayload {
  state: string;
  message: string;
}

export class HnbFrame {
  readonly type: HnbFrameType;
  readonly flags: number;
  readonly sequence: number;
  readonly payload: Uint8Array;

  constructor(type: HnbFrameType, flags: number, sequence: number, payload: Uint8Array) {
    this.type = type;
    this.flags = flags;
    this.sequence = sequence;
    this.payload = payload;
  }
}

export function encodeJsonPayload(value: Object): Uint8Array {
  const text: string | undefined = JSON.stringify(value);
  if (text === undefined) {
    throw new Error('HNB control payload cannot be serialized as JSON');
  }
  const encoded: Uint8Array = encodeUtf8(text);
  if (encoded.length > HNB_MAX_CONTROL_PAYLOAD) {
    throw new Error('HNB control payload exceeds 16 KiB');
  }
  return encoded;
}

export function isSupportedMtu(mtu: number): boolean {
  return Number.isInteger(mtu) && mtu >= HNB_MIN_MTU && mtu <= HNB_MAX_MTU;
}

export function decodeJsonPayload<T>(payload: Uint8Array): T {
  if (payload.length > HNB_MAX_CONTROL_PAYLOAD) {
    throw new Error('HNB control payload exceeds 16 KiB');
  }
  const text: string = decodeUtf8(payload);
  return JSON.parse(text) as T;
}

// These small protocol-local UTF-8 helpers behave identically in the device
// runtime and DevEco host unit-test runtime. They also reject malformed input
// instead of silently replacing bytes in a control message.
export function encodeUtf8(text: string): Uint8Array {
  let byteLength: number = 0;
  for (let index: number = 0; index < text.length; index++) {
    const codePoint: number = codePointAt(text, index);
    if (codePoint > 0xFFFF) {
      index += 1;
    }
    if (codePoint <= 0x7F) {
      byteLength += 1;
    } else if (codePoint <= 0x7FF) {
      byteLength += 2;
    } else if (codePoint <= 0xFFFF) {
      byteLength += 3;
    } else {
      byteLength += 4;
    }
  }

  const result: Uint8Array = new Uint8Array(byteLength);
  let output: number = 0;
  for (let index: number = 0; index < text.length; index++) {
    const codePoint: number = codePointAt(text, index);
    if (codePoint > 0xFFFF) {
      index += 1;
    }
    if (codePoint <= 0x7F) {
      result[output++] = codePoint;
    } else if (codePoint <= 0x7FF) {
      result[output++] = 0xC0 | Math.floor(codePoint / 64);
      result[output++] = 0x80 | (codePoint % 64);
    } else if (codePoint <= 0xFFFF) {
      result[output++] = 0xE0 | Math.floor(codePoint / 4096);
      result[output++] = 0x80 | (Math.floor(codePoint / 64) % 64);
      result[output++] = 0x80 | (codePoint % 64);
    } else {
      result[output++] = 0xF0 | Math.floor(codePoint / 262144);
      result[output++] = 0x80 | (Math.floor(codePoint / 4096) % 64);
      result[output++] = 0x80 | (Math.floor(codePoint / 64) % 64);
      result[output++] = 0x80 | (codePoint % 64);
    }
  }
  return result;
}

export function decodeUtf8(bytes: Uint8Array): string {
  let result: string = '';
  let index: number = 0;
  while (index < bytes.length) {
    const first: number = bytes[index];
    let codePoint: number;
    let continuationCount: number;
    let minimum: number;
    if (first <= 0x7F) {
      codePoint = first;
      continuationCount = 0;
      minimum = 0;
    } else if (first >= 0xC2 && first <= 0xDF) {
      codePoint = first & 0x1F;
      continuationCount = 1;
      minimum = 0x80;
    } else if (first >= 0xE0 && first <= 0xEF) {
      codePoint = first & 0x0F;
      continuationCount = 2;
      minimum = 0x800;
    } else if (first >= 0xF0 && first <= 0xF4) {
      codePoint = first & 0x07;
      continuationCount = 3;
      minimum = 0x10000;
    } else {
      throw new Error('HNB payload is not valid UTF-8');
    }
    if (index + continuationCount >= bytes.length) {
      throw new Error('HNB payload contains truncated UTF-8');
    }
    for (let continuation: number = 1; continuation <= continuationCount; continuation++) {
      const next: number = bytes[index + continuation];
      if ((next & 0xC0) !== 0x80) {
        throw new Error('HNB payload is not valid UTF-8');
      }
      codePoint = codePoint * 64 + (next & 0x3F);
    }
    if (codePoint < minimum || codePoint > 0x10FFFF || (codePoint >= 0xD800 && codePoint <= 0xDFFF)) {
      throw new Error('HNB payload contains an invalid Unicode code point');
    }
    if (codePoint <= 0xFFFF) {
      result += String.fromCharCode(codePoint);
    } else {
      const adjusted: number = codePoint - 0x10000;
      result += String.fromCharCode(0xD800 + Math.floor(adjusted / 1024));
      result += String.fromCharCode(0xDC00 + (adjusted % 1024));
    }
    index += continuationCount + 1;
  }
  return result;
}

export function encodeFrame(type: HnbFrameType, sequence: number, payload: Uint8Array): Uint8Array {
  validateFrame(type, 0, sequence, payload.length);
  const result: Uint8Array = new Uint8Array(HNB_HEADER_SIZE + payload.length);
  result[0] = 0x48;
  result[1] = 0x4E;
  result[2] = 0x42;
  result[3] = 0x31;
  result[4] = HNB_VERSION;
  result[5] = type;
  writeUint16(result, 6, 0);
  writeUint32(result, 8, payload.length);
  writeUint32(result, 12, sequence);
  result.set(payload, HNB_HEADER_SIZE);
  return result;
}

// HnbFrameDecoder retains incomplete TCP bytes and emits every complete frame,
// handling both fragmentation and multiple frames in one socket callback.
export class HnbFrameDecoder {
  private pending: Uint8Array = new Uint8Array(0);

  push(chunk: ArrayBuffer): HnbFrame[] {
    const incoming: Uint8Array = new Uint8Array(chunk);
    const combined: Uint8Array = new Uint8Array(this.pending.length + incoming.length);
    combined.set(this.pending, 0);
    combined.set(incoming, this.pending.length);
    this.pending = combined;

    const frames: HnbFrame[] = [];
    let offset: number = 0;
    while (this.pending.length - offset >= HNB_HEADER_SIZE) {
      this.validateMagicAndVersion(offset);
      const type: HnbFrameType = this.pending[offset + 5] as HnbFrameType;
      const flags: number = readUint16(this.pending, offset + 6);
      const payloadLength: number = readUint32(this.pending, offset + 8);
      const sequence: number = readUint32(this.pending, offset + 12);
      validateFrame(type, flags, sequence, payloadLength);
      const frameLength: number = HNB_HEADER_SIZE + payloadLength;
      if (this.pending.length - offset < frameLength) {
        break;
      }
      const payload: Uint8Array = this.pending.slice(offset + HNB_HEADER_SIZE, offset + frameLength);
      frames.push(new HnbFrame(type, flags, sequence, payload));
      offset += frameLength;
    }
    if (offset > 0) {
      this.pending = this.pending.slice(offset);
    }
    return frames;
  }

  reset(): void {
    this.pending = new Uint8Array(0);
  }

  private validateMagicAndVersion(offset: number): void {
    if (this.pending[offset] !== 0x48 || this.pending[offset + 1] !== 0x4E ||
      this.pending[offset + 2] !== 0x42 || this.pending[offset + 3] !== 0x31) {
      throw new Error('HNB frame has invalid magic');
    }
    if (this.pending[offset + 4] !== HNB_VERSION) {
      throw new Error(`HNB version ${this.pending[offset + 4]} is unsupported`);
    }
  }
}

function validateFrame(type: HnbFrameType, flags: number, sequence: number, payloadLength: number): void {
  if (!isKnownType(type)) {
    throw new Error(`Unknown HNB frame type ${type}`);
  }
  if (flags !== 0) {
    throw new Error('HNB/1 flags must be zero');
  }
  if (sequence === 0) {
    throw new Error('HNB/1 sequence zero is reserved');
  }
  const limit: number = type === HnbFrameType.IP_PACKET ? HNB_MAX_PACKET_PAYLOAD : HNB_MAX_CONTROL_PAYLOAD;
  if (payloadLength > limit) {
    throw new Error(`HNB payload ${payloadLength} exceeds ${limit}`);
  }
}

function isKnownType(type: HnbFrameType): boolean {
  return type === HnbFrameType.HELLO || type === HnbFrameType.HELLO_ACK || type === HnbFrameType.ERROR ||
    type === HnbFrameType.STOP_REQUEST || type === HnbFrameType.STOP_ACK || type === HnbFrameType.DATA_HELLO ||
    type === HnbFrameType.DATA_ACK || type === HnbFrameType.VPN_STATUS || type === HnbFrameType.IP_PACKET || type === HnbFrameType.PING ||
    type === HnbFrameType.PONG;
}

function codePointAt(text: string, index: number): number {
  const first: number = text.charCodeAt(index);
  if (first >= 0xD800 && first <= 0xDBFF && index + 1 < text.length) {
    const second: number = text.charCodeAt(index + 1);
    if (second >= 0xDC00 && second <= 0xDFFF) {
      return 0x10000 + (first - 0xD800) * 1024 + (second - 0xDC00);
    }
  }
  if (first >= 0xD800 && first <= 0xDFFF) {
    return 0xFFFD;
  }
  return first;
}

function readUint16(bytes: Uint8Array, offset: number): number {
  return bytes[offset] * 256 + bytes[offset + 1];
}

function readUint32(bytes: Uint8Array, offset: number): number {
  return bytes[offset] * 16777216 + bytes[offset + 1] * 65536 + bytes[offset + 2] * 256 + bytes[offset + 3];
}

function writeUint16(bytes: Uint8Array, offset: number, value: number): void {
  bytes[offset] = Math.floor(value / 256) % 256;
  bytes[offset + 1] = value % 256;
}

function writeUint32(bytes: Uint8Array, offset: number, value: number): void {
  bytes[offset] = Math.floor(value / 16777216) % 256;
  bytes[offset + 1] = Math.floor(value / 65536) % 256;
  bytes[offset + 2] = Math.floor(value / 256) % 256;
  bytes[offset + 3] = value % 256;
}
