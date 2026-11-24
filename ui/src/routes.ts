// Typed calls to the module's routes. The client is generated from
// openapi.json by `make client`.
import type { FaultText } from '@prusa/connect-ui/i18n';
import type { ModuleHost } from '@prusa/connect-ui/slots';
import { bind, type components, type operations } from 'otel-exporter-client';

type Body<K extends keyof operations> =
  operations[K]['responses'][200]['content']['application/json'];

export type Status = Body<'read-export-status'>;
export type Outcome = components['schemas']['Outcome'];
export type DeviceExport = Body<'read-device-export'>;
export type ExportResult = Body<'send-export'>;

/** How often open views refresh. */
export const REFRESH_MS = 10_000;

export const routesOn = (host: ModuleHost) => {
  const routes = bind(host.client);

  return {
    status: (signal?: AbortSignal): Promise<Status> =>
      routes.readExportStatus({}, signal),
    device: (id: string, signal?: AbortSignal): Promise<DeviceExport> =>
      routes.readDeviceExport({ path: { id } }, signal),
    exportNow: (signal?: AbortSignal): Promise<ExportResult> =>
      routes.sendExport({}, signal),
  };
};

export type Health = 'unconfigured' | 'waiting' | 'healthy' | 'failing';

/** Failing if the stream or an export is failing, healthy once metrics have exported. */
export const healthOf = (status: Status): Health => {
  if (!status.configured) return 'unconfigured';

  const outcomes = [status.stream, ...Object.values(status.exports)];
  if (outcomes.some((o) => o.failing_since !== undefined)) return 'failing';

  return status.exports.metrics?.ok ? 'healthy' : 'waiting';
};

export const healthBadge = {
  unconfigured: 'secondary',
  waiting: 'info',
  healthy: 'success',
  failing: 'destructive',
} as const satisfies Record<Health, string>;

/** The host returns this code while the module is stopped, e.g. without otlp_endpoint. */
export const isNotRunning = (fault: FaultText): boolean =>
  fault.code === 'core.internal/RESOURCE_NOT_READY';
