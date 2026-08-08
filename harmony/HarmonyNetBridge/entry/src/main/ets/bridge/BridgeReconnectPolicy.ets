const BRIDGE_RECONNECT_BASE_DELAY_MS: number = 500;
const BRIDGE_RECONNECT_MAX_DELAY_MS: number = 4000;

// The foreground control connection should notice a Mac daemon that starts
// later without polling in a tight loop while the daemon is unavailable.
export function bridgeReconnectDelay(attempt: number): number {
  if (attempt <= 1) {
    return BRIDGE_RECONNECT_BASE_DELAY_MS;
  }
  let delay: number = BRIDGE_RECONNECT_BASE_DELAY_MS;
  for (let index: number = 1; index < attempt; index++) {
    delay = Math.min(delay * 2, BRIDGE_RECONNECT_MAX_DELAY_MS);
  }
  return delay;
}

export function shouldScheduleBridgeReconnect(autoReconnectEnabled: boolean, hasClient: boolean,
  connectionState: string): boolean {
  return autoReconnectEnabled && !hasClient && connectionState !== 'CONNECTING' && connectionState !== 'CONNECTED';
}
