const DNS_HEADER_SIZE: number = 12;
const DNS_TCP_PREFIX_SIZE: number = 2;

export const DIAGNOSTIC_DNS_TRANSACTION_ID: number = 0x484E;

// A deterministic A/IN query for example.com. The diagnostic checks the
// transport path and resolver behavior; it never logs DNS payloads.
export function createDiagnosticDnsQuery(): Uint8Array {
  return new Uint8Array([
    0x48, 0x4E, // transaction ID
    0x01, 0x00, // recursion desired
    0x00, 0x01, // one question
    0x00, 0x00, // no answers
    0x00, 0x00, // no authority records
    0x00, 0x00, // no additional records
    0x07, 0x65, 0x78, 0x61, 0x6D, 0x70, 0x6C, 0x65, // example
    0x03, 0x63, 0x6F, 0x6D, // com
    0x00,
    0x00, 0x01, // A
    0x00, 0x01 // IN
  ]);
}

export function frameDnsOverTcp(query: Uint8Array): Uint8Array {
  if (query.length > 65535) {
    throw new Error('DNS query is too large for TCP framing');
  }
  const framed: Uint8Array = new Uint8Array(DNS_TCP_PREFIX_SIZE + query.length);
  framed[0] = Math.floor(query.length / 256);
  framed[1] = query.length % 256;
  framed.set(query, DNS_TCP_PREFIX_SIZE);
  return framed;
}

export function validateDiagnosticDnsResponse(message: Uint8Array): number {
  if (message.length < DNS_HEADER_SIZE) {
    throw new Error('DNS response is shorter than its header');
  }
  const transactionId: number = message[0] * 256 + message[1];
  if (transactionId !== DIAGNOSTIC_DNS_TRANSACTION_ID) {
    throw new Error('DNS response transaction ID does not match');
  }
  const flags: number = message[2] * 256 + message[3];
  if ((flags & 0x8000) === 0) {
    throw new Error('DNS payload is not a response');
  }
  const responseCode: number = flags & 0x000F;
  if (responseCode !== 0) {
    throw new Error(`DNS resolver returned response code ${responseCode}`);
  }
  const answerCount: number = message[6] * 256 + message[7];
  if (answerCount === 0) {
    throw new Error('DNS response contains no answers');
  }
  return answerCount;
}

function appendBytes(existing: Uint8Array, chunk: Uint8Array): Uint8Array {
  const combined: Uint8Array = new Uint8Array(existing.length + chunk.length);
  combined.set(existing, 0);
  combined.set(chunk, existing.length);
  return combined;
}

// TCP is a byte stream, so a DNS response may arrive in any number of socket
// callbacks. This decoder waits for the RFC 1035 two-byte length prefix and
// returns exactly one complete response.
export class DnsTcpResponseDecoder {
  private buffered: Uint8Array = new Uint8Array(0);

  push(chunk: ArrayBuffer): Uint8Array | undefined {
    this.buffered = appendBytes(this.buffered, new Uint8Array(chunk));
    if (this.buffered.length < DNS_TCP_PREFIX_SIZE) {
      return undefined;
    }
    const payloadLength: number = this.buffered[0] * 256 + this.buffered[1];
    if (payloadLength < DNS_HEADER_SIZE) {
      throw new Error('DNS TCP response has an invalid length');
    }
    const frameLength: number = DNS_TCP_PREFIX_SIZE + payloadLength;
    if (this.buffered.length < frameLength) {
      return undefined;
    }
    return this.buffered.slice(DNS_TCP_PREFIX_SIZE, frameLength);
  }
}
