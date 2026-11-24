<!-- Section on a printer's Status tab. -->
<script lang="ts">
  import { describeFault, type FaultText } from '@prusa/connect-ui/i18n';
  import { Badge, FaultAlert, Skeleton } from '@prusa/connect-ui/primitives';
  import { onMount } from 'svelte';

  import { heldHost } from './host';
  import {
    type DeviceExport,
    isNotRunning,
    REFRESH_MS,
    routesOn,
  } from './routes';

  interface Props {
    deviceId: string;
  }

  let { deviceId }: Props = $props();

  const host = heldHost();
  const t = host.t;
  const routes = routesOn(host);

  let exportState = $state<DeviceExport | undefined>();
  let fault = $state<FaultText | undefined>();

  const ended = new AbortController();
  const signal = AbortSignal.any([ended.signal, host.signal]);

  const read = async (): Promise<void> => {
    try {
      exportState = await routes.device(deviceId, signal);
      fault = undefined;
    } catch (thrown: unknown) {
      if (!signal.aborted) fault = describeFault(thrown);
    }
  };

  onMount(() => {
    void read();
    const timer = setInterval(() => void read(), REFRESH_MS);

    return () => {
      clearInterval(timer);
      ended.abort();
    };
  });
</script>

{#if fault && isNotRunning(fault)}
  <p class="text-sm text-muted-foreground">
    {t('otel-exporter.device.notRunning')}
  </p>
{:else if fault}
  <FaultAlert {fault} />
{:else if exportState === undefined}
  <Skeleton class="h-6 w-48" />
{:else if !exportState.configured}
  <p class="text-sm text-muted-foreground">
    {t('otel-exporter.device.unconfigured')}
  </p>
{:else if !exportState.exported}
  <p class="text-sm text-muted-foreground">
    {t('otel-exporter.device.unknown')}
  </p>
{:else if !exportState.connected}
  <div class="flex items-center gap-2 text-sm">
    <Badge variant="secondary">{t('otel-exporter.device.paused')}</Badge>
    <span class="text-muted-foreground"
      >{t('otel-exporter.device.pausedDetail')}</span
    >
  </div>
{:else}
  <div class="flex items-center gap-2 text-sm">
    <Badge variant="success">{t('otel-exporter.device.exporting')}</Badge>
    <span class="text-muted-foreground"
      >{t('otel-exporter.device.values', { count: exportState.values })}</span
    >
  </div>
{/if}
