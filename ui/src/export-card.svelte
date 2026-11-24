<!-- Dashboard card. -->
<script lang="ts">
  import {
    describeFault,
    type FaultText,
    formatDateTime,
  } from '@prusa/connect-ui/i18n';
  import { Badge } from '@prusa/connect-ui/primitives';
  import type { CardProps } from '@prusa/connect-ui/slots';
  import { onMount } from 'svelte';

  import { heldHost } from './host';
  import {
    healthBadge,
    healthOf,
    isNotRunning,
    REFRESH_MS,
    routesOn,
    type Status,
  } from './routes';

  let { title, size, editing }: CardProps = $props();

  const host = heldHost();
  const t = host.t;
  const routes = routesOn(host);

  let status = $state<Status | undefined>();
  let fault = $state<FaultText | undefined>();

  const health = $derived(status ? healthOf(status) : undefined);
  const heading = $derived(
    title === '' ? t('otel-exporter.card.title') : title,
  );
  const lastExport = $derived(status?.exports.metrics?.last_success);

  const ended = new AbortController();
  const signal = AbortSignal.any([ended.signal, host.signal]);

  const read = async (): Promise<void> => {
    try {
      status = await routes.status(signal);
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

<div class="flex h-full flex-col gap-2 p-3">
  <h3 class="truncate text-sm font-medium">{heading}</h3>
  {#if fault && isNotRunning(fault)}
    <Badge variant="secondary" class="w-fit"
      >{t('otel-exporter.health.notRunning')}</Badge
    >
  {:else if fault}
    <p class="text-sm text-destructive">{fault.title}</p>
  {:else if health !== undefined && status !== undefined}
    <Badge variant={healthBadge[health]} class="w-fit"
      >{t(`otel-exporter.health.${health}`)}</Badge
    >
    {#if size.h > 2 && !editing}
      {#if lastExport}
        <p class="text-sm text-muted-foreground">
          {t('otel-exporter.card.lastExport', {
            at: formatDateTime(lastExport),
          })}
        </p>
      {/if}
      {#if status.configured}
        <p class="text-sm text-muted-foreground">
          {t('otel-exporter.card.printers', { count: status.devices })}
        </p>
      {/if}
    {/if}
  {/if}
</div>
