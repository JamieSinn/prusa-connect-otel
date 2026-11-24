# OpenTelemetry exporter for Prusa Connect

A [Prusa Connect](https://github.com/prusa3d/prusa-connect) module that exports
printer telemetry as OTLP metrics and finished prints as spans. It works with
an OpenTelemetry Collector or any backend that accepts OTLP.

It reads the Prusa Connect event stream rather than polling printers. For a
single PrusaLink printer without Prusa Connect, see
[otel-prusa-link](https://github.com/JamieSinn/otel-prusa-link).

- Extension id: `com.github.aux.jamiesinn.connect-otel` (from the module path `github.com/jamiesinn/connect-otel`)
- Permission: `observe`
- Network access: only the host in `otlp_endpoint`

## Metrics

Every numeric, boolean (1/0) or state value in a printer's state tree becomes
a gauge named after its path. Numeric path segments become attributes:

| State path               | Metric                                      | Attributes             |
|--------------------------|---------------------------------------------|------------------------|
| `/tools/0/nozzle_temp`   | `prusa.connect.tools.nozzle_temp` (Cel)     | `prusa.tools.index=0`  |
| `/temperature/bed`       | `prusa.connect.temperature.bed` (Cel)       |                        |
| `/tools/0/print_fan_rpm` | `prusa.connect.tools.print_fan_rpm` ({rpm}) | `prusa.tools.index=0`  |
| `/job/progress`          | `prusa.connect.job.progress` (%)            |                        |
| `/state`                 | `prusa.connect.state` = 1                   | `prusa.state=PRINTING` |

All points carry `prusa.device.id`, plus `prusa.device.name`,
`prusa.device.vendor` and `prusa.device.model` when known.

- The available paths depend on the printer driver.
- Strings other than `/state` and `/job/state` are skipped, as are paths
  ending in `id` (such as `/job/id`).
- Units are guessed from the path name.
- At most 256 metric names are created; values for further paths are dropped
  and counted in `GET /status`.

Current values are loaded from Prusa Connect at startup and whenever the event
stream reports a gap. After that, the stream supplies changes.

`prusa.connect.device.connected` is 1 while the printer is connected and 0
otherwise. A disconnected printer stops reporting its other gauges.

## Prints

Each finished print is exported as a `print_job` span using the start and end
times Prusa Connect recorded. Failed prints get an error status. Span
attributes: `prusa.job.id`, `prusa.job.outcome` (`finished`, `failed`,
`cancelled`, `unknown`), `prusa.job.display_name`, `prusa.job.path`,
`prusa.job.machine_state`, `prusa.job.progress`, and, if the printer reports
them, `prusa.job.time_printing_s` and `prusa.job.filament_used_mm`.

| Metric                            | Type          | Attributes                  |
|-----------------------------------|---------------|-----------------------------|
| `prusa.connect.job.settled`       | counter       | device, `prusa.job.outcome` |
| `prusa.connect.job.duration`      | histogram (s) | device, `prusa.job.outcome` |
| `prusa.connect.job.filament_used` | counter (mm)  | device                      |

Prints with no recorded start time are not added to the duration histogram.
Prints that finish while the module isn't running are not exported.

## Configuration

| Key                    | Default              | Description                                                                                                                    |
|------------------------|----------------------|--------------------------------------------------------------------------------------------------------------------------------|
| `otlp_endpoint`        | (required)           | `host:port` or URL. A URL path is used as a prefix for `/v1/metrics` and `/v1/traces`. `https://` uses TLS, `http://` doesn't. |
| `otlp_protocol`        | `http/protobuf`      | or `grpc` (ignores the URL path)                                                                                               |
| `otlp_headers`         |                      | Secret. Comma-separated `key=value` pairs, e.g. `Authorization=Bearer <token>`                                                 |
| `otlp_insecure`        | `false`              | Disable TLS for an endpoint without a scheme                                                                                   |
| `otlp_tls_skip_verify` | `false`              | Skip certificate verification                                                                                                  |
| `service_name`         | `prusa-connect-otel` | `service.name` resource attribute                                                                                              |
| `export_interval`      | `10s`                | Metric export interval                                                                                                         |
| `metric_prefix`        | `prusa.connect`      | Metric name prefix                                                                                                             |

Set these on the module's Settings tab under **Settings → Extensions**, or
through the resources API. Don't put them under `resources.sink.otel-exporter`
in `config.yaml`: the module's declaration lives in the server's overlay, and
the server rejects a second one.

```sh
NS=/api/v1/resources/sink/otel-exporter
ETAG=$(curl -s -D - -o decl.json -b cookies "$BASE$NS" | awk -F': ' 'tolower($1)=="etag"{print $2}' | tr -d '\r')
jq '.config = {
      "otlp_endpoint": "https://collector.example.com:4318",
      "otlp_headers":  "Authorization=Bearer <token>"
    }' decl.json > put.json
curl -s -b cookies -X PUT "$BASE$NS" -H 'content-type: application/json' -H "If-Match: $ETAG" --data @put.json
```

The module restarts with the new settings. Until `otlp_endpoint` is set, the
instance fails with `RESOURCE_PLUGIN_SETTINGS`.

The manifest allows outbound connections to the `otlp_endpoint` host on ports
443, 4317 and 4318. If your collector uses another port, add it to
`egress` in `module.yaml`.

## Status

`GET /api/v1/modules/com.github.aux.jamiesinn.connect-otel/status`:

```json
{
  "configured": true,
  "endpoint": "https://collector.example.com:4318",
  "protocol": "http/protobuf",
  "stream": {
    "ok": true,
    "last_success": "2026-11-23T21:50:02Z"
  },
  "last_window": "2026-11-23T21:50:02Z",
  "exports": {
    "metrics": {
      "ok": false,
      "last_success": "2026-11-23T21:41:12Z",
      "failing_since": "2026-11-23T21:41:22Z",
      "error": "failed to send metrics to ...: 401 Unauthorized"
    },
    "traces": {
      "ok": true,
      "last_success": "2026-11-23T21:49:30Z"
    }
  },
  "devices": 3,
  "values": 51,
  "metrics": 22,
  "dropped_values": 0
}
```

An error is shown until the next successful export. The exporter retries 429,
502, 503 and 504 responses for up to a minute before reporting a failure. The
server log gets one line when exports start failing and one when they recover.

## In the app

- **Settings → Extensions → OpenTelemetry exporter → Export status**: endpoint,
  stream and export health, counts, the last error, and an **Export now**
  button.
- **Printer Status tab**: whether the printer is exported, and its value count.
- **Dashboards**: an *OpenTelemetry* card showing overall health.

## Development

Requires Go 1.26.11, Node 22.12+ and pnpm (`corepack enable`). The Prusa
Connect SDK and `@prusa` packages aren't published yet, so everything builds
against the `.core` submodule.

```sh
git clone --recurse-submodules https://github.com/JamieSinn/prusa-connect-otel
make check   # Go: gofmt, vet, golangci-lint, go test -race
             # client/ is up to date with openapi.json
             # UI: eslint, prettier, svelte-check, build
```

| Path                |                                                                             |
|---------------------|-----------------------------------------------------------------------------|
| `*.go`, `internal/` | the exporter process                                                        |
| `openapi.json`      | routes: `GET /status`, `GET /devices/{id}`, `POST /export`                  |
| `client/`           | typed client for the routes, generated by `make client` (commit the output) |
| `ui/`               | web UI, in Svelte on Prusa Connect's component library                      |
| `install.sh`        | builds everything and installs it into a local Prusa Connect                |

The Go tests run the module against the SDK's host handlers and an in-process
OTLP receiver.

### Running it locally

Start the Prusa Connect demo (simulated printers) with the module installed:

```sh
cd .core && go run ./cmd/new-module -start demo -dir ..
```

This runs `install.sh`, starts the core (port 8080 or the next free one) with
the UI dev server in front, and prints the URL and the demo login. Changes under
`ui/src` reload live. After changing Go code or `module.yaml`, run
`sh install.sh` and restart, since the core loads modules at startup. Use
`-start app` for the real app instead of the demo.

The module stays stopped until `otlp_endpoint` is set, on its Settings tab or
via the API calls under [Configuration](#configuration). Without a collector,
any HTTP server that returns `200` for `POST /v1/metrics` and `/v1/traces`
will do.

To install into a Prusa Connect you already run, build, add it with that
server's config, and restart the server:

```sh
make ui-build
go build -o ui/dist/otel-exporter_$(go env GOOS)_$(go env GOARCH) .
go -C .core run ./cmd/prusa-connect module add github.com/jamiesinn/connect-otel@<version> \
  -config /path/to/its/config.yaml -from "$PWD/ui/dist" -approve
```

### Updating Prusa Connect

Check out the new tag in `.core`, then update `core-ref` in
`.github/workflows/ci.yml` and `release.yml` and `require.core` in
`module.yaml`. CI fails if the submodule and `core-ref` disagree. Then run
`make client` in case the generator changed.

## Releasing

Releases are managed by
[release-please](https://github.com/googleapis/release-please) using
[Conventional Commits](https://www.conventionalcommits.org/). Below 1.0,
`fix:` bumps the patch version, and both `feat:` and breaking changes bump the
minor version.

Pushes to `main` keep a release PR up to date. It bumps the version in
`module.yaml`, `main.go` and `openapi.json` and updates `CHANGELOG.md`.
Merging it tags the release and publishes it to the Prusa Plugin Marketplace,
where it's reviewed before operators can install it.

To force a version, add `Release-As: x.y.z` to a commit message footer.
