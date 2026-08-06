export interface GateProbeResult {
  status: number;
  length: number;
  version: number;
  protocol: number;
  destination: string;
}

export const probeTun: (tunFd: number, timeoutMs: number) => GateProbeResult;

export interface DataConnectResult {
  status: number;
  fd: number;
}

export interface PacketPumpStatus {
  connected: boolean;
  running: boolean;
  errorCode: number;
  dataFd: number;
  packetsFromTun: number;
  bytesFromTun: number;
  packetsToTun: number;
  bytesToTun: number;
  droppedPackets: number;
}

export const connectData: (address: string, port: number, sessionToken: string, timeoutMs: number) => DataConnectResult;
export const startPacketPump: (tunFd: number) => number;
export const stopPacketPump: () => void;
export const getPacketPumpStatus: () => PacketPumpStatus;
