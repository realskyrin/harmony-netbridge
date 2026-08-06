const RECONNECT_BASE_DELAY_MS: number = 1000;
const RECONNECT_MAX_DELAY_MS: number = 10000;

// reconnectDelay applies bounded exponential backoff. An established session
// resets the caller's attempt count, so transient outages recover quickly while
// a missing Mac daemon does not create a tight retry loop.
export function reconnectDelay(attempt: number): number {
  if (attempt <= 1) {
    return RECONNECT_BASE_DELAY_MS;
  }
  let delay: number = RECONNECT_BASE_DELAY_MS;
  for (let index: number = 1; index < attempt; index++) {
    delay = Math.min(delay * 2, RECONNECT_MAX_DELAY_MS);
  }
  return delay;
}
