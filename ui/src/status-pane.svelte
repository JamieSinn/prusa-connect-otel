<!-- Settings → Extensions → OpenTelemetry exporter → Export status -->
<script lang="ts">
  import {
    describeFault,
    type FaultText,
    formatDateTime,
  } from '@prusa/connect-ui/i18n';
  import {
    Badge,
    Button,
    FaultAlert,
    ListingSkeleton,
    SettingsPane,
    toast,
  } from '@prusa/connect-ui/primitives';
  import { onMount } from 'svelte';

  import { heldHost } from './host';
  import {
    healthBadge,
    healthOf,
    isNotRunning,
    type Outcome,
    REFRESH_MS,
    routesOn,
    type Status,
  } from './routes';

  const host = heldHost();
  const t = host.t;
  const routes = routesOn(host);

  let status = $state<Status | undefined>();
  let fault = $state<FaultText | undefined>();
  let exporting = $state(false);

  const health = $derived(status ? healthOf(status) : undefined);

  // Aborted on unmount or sign-out.
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

  const exportNow = async (): Promise<void> => {
    exporting = true;
    try {
      const result = await routes.exportNow(signal);
      if (result.ok) {
        toast.success(t('otel-exporter.export.sent'));
      } else {
        toast.error(
          t('otel-exporter.export.failed'),
          result.error === undefined ? {} : { description: result.error },
        );
      }
    } catch (thrown: unknown) {
      if (!signal.aborted) toast.error(describeFault(thrown).title);
    } finally {
      exporting = false;
      void read();
    }
  };

  const outcomeText = (outcome: Outcome | undefined): string => {
    if (
      outcome === undefined ||
      (outcome.last_success === undefined &&
        outcome.failing_since === undefined)
    ) {
      return t('otel-exporter.outcome.none');
    }
    if (outcome.failing_since !== undefined) {
      return t('otel-exporter.outcome.failing', {
        since: formatDateTime(outcome.failing_since),
      });
    }

    return t('otel-exporter.outcome.ok', {
      at: formatDateTime(outcome.last_success ?? ''),
    });
  };
</script>

<SettingsPane
  title={t('otel-exporter.status.title')}
  description={t('otel-exporter.status.description')}
  edged
>
  {#if fault && isNotRunning(fault)}
    <p class="text-sm text-muted-foreground">
      {t('otel-exporter.status.notRunning')}
    </p>
  {:else if fault}
    <FaultAlert {fault} />
  {:else if status === undefined || health === undefined}
    <ListingSkeleton label={t('otel-exporter.status.reading')} rows={4} />
  {:else}
    <div class="flex flex-col gap-4 text-sm">
      <Badge variant={healthBadge[health]} class="w-fit"
        >{t(`otel-exporter.health.${health}`)}</Badge
      >

      {#if !status.configured}
        <p class="text-muted-foreground">
          {t('otel-exporter.status.unconfigured')}
        </p>
      {:else}
        <dl class="grid w-fit grid-cols-2 gap-x-8 gap-y-2">
          <dt class="text-muted-foreground">
            {t('otel-exporter.status.endpoint')}
          </dt>
          <dd class="font-mono break-all">{status.endpoint}</dd>
          <dt class="text-muted-foreground">
            {t('otel-exporter.status.protocol')}
          </dt>
          <dd class="font-mono">{status.protocol}</dd>
          <dt class="text-muted-foreground">
            {t('otel-exporter.status.stream')}
          </dt>
          <dd>{outcomeText(status.stream)}</dd>
          <dt class="text-muted-foreground">
            {t('otel-exporter.status.metrics')}
          </dt>
          <dd>{outcomeText(status.exports.metrics)}</dd>
          <dt class="text-muted-foreground">
            {t('otel-exporter.status.traces')}
          </dt>
          <dd>{outcomeText(status.exports.traces)}</dd>
          <dt class="text-muted-foreground">
            {t('otel-exporter.status.printers')}
          </dt>
          <dd>{status.devices}</dd>
          <dt class="text-muted-foreground">
            {t('otel-exporter.status.values')}
          </dt>
          <dd>{status.values}</dd>
          <dt class="text-muted-foreground">
            {t('otel-exporter.status.metricNames')}
          </dt>
          <dd>{status.metrics}</dd>
        </dl>

        {#each [status.stream, status.exports.metrics, status.exports.traces] as outcome, i (i)}
          {#if outcome?.error}
            <p class="font-mono text-xs break-all text-destructive">
              {outcome.error}
            </p>
          {/if}
        {/each}

        {#if status.dropped_values > 0}
          <p class="text-warning">
            {t('otel-exporter.status.dropped', {
              count: status.dropped_values,
            })}
          </p>
        {/if}
      {/if}
    </div>
  {/if}

  {#snippet actions()}
    <Button onclick={exportNow} disabled={exporting || !status?.configured}>
      {t(
        exporting
          ? 'otel-exporter.export.sending'
          : 'otel-exporter.export.button',
      )}
    </Button>
  {/snippet}
</SettingsPane>
