export const VPN_DESTROY_TIMEOUT_MS: number = 2000;
export const VPN_CONTROL_STEP_TIMEOUT_MS: number = 1000;
export const VPN_STATUS_PUBLISH_TIMEOUT_MS: number = 1000;
export const VPN_STOP_WATCHDOG_TIMEOUT_MS: number = 6000;
export const VPN_FORCE_STOP_TIMEOUT_MS: number = 2000;

// HarmonyOS networking cleanup APIs are allowed to finish asynchronously, but
// a platform Promise that never settles must not leave the product UI stuck in
// STOPPING forever. The original operation keeps running after the deadline;
// callers can continue with the remaining idempotent cleanup steps.
export function completeWithin(operation: Promise<void>, timeoutMs: number, label: string): Promise<void> {
  return new Promise<void>((resolve: () => void, reject: (error: Error) => void) => {
    let settled: boolean = false;
    const timer: number = setTimeout(() => {
      if (settled) {
        return;
      }
      settled = true;
      reject(new Error(`${label} timed out after ${timeoutMs} ms`));
    }, timeoutMs);
    operation.then(() => {
      if (settled) {
        return;
      }
      settled = true;
      clearTimeout(timer);
      resolve();
    }).catch((error: Error) => {
      if (settled) {
        return;
      }
      settled = true;
      clearTimeout(timer);
      reject(error);
    });
  });
}
