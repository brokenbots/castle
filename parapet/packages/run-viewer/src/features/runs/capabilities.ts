/**
 * CRI-186 guards matrix extension: run-status gating gains a capability
 * axis so the same control components drive both hosts. Parapet (castle
 * host) exposes the control RPCs; the standalone run-viewer served from a
 * loopback data source does not (until CRI-255 ships its control RPC).
 */
export interface RunCapabilities {
  /** Whether control RPCs (pause/resume/stop/approve/reject) are reachable. */
  controls: boolean;
}

/** Capabilities of the castle host: the control RPCs exist today. */
export const CASTLE_RUN_CAPABILITIES: RunCapabilities = { controls: true };

/** Capabilities of a host without control RPCs (standalone mode). */
export const NO_CONTROL_CAPABILITIES: RunCapabilities = { controls: false };

/** Tooltip shown on control buttons when the host has no control RPC. */
export const NO_CONTROLS_TOOLTIP = 'Controls unavailable — this host has no control RPC.';

export function hasControls(capabilities: RunCapabilities | undefined): boolean {
  // Default to "controls available": castle-host behavior is unchanged
  // when a host does not pass the probe result.
  return capabilities?.controls ?? true;
}