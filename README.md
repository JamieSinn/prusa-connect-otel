# OpenTelemetry exporter for Prusa Connect

A [Prusa Connect](https://github.com/prusa3d/prusa-connect) module that exports
printer telemetry as OTLP metrics and finished prints as spans. It works with
any OTLP backend: an OpenTelemetry Collector, Dynatrace, Grafana, Honeycomb,
and so on.

It reads the Prusa Connect event stream rather than polling printers. For a
single PrusaLink printer without Prusa Connect, see
[otel-prusa-link](https://github.com/JamieSinn/otel-prusa-link).

- Module path: `prusa-connect-otel.jamiesinn.ca`
- Namespace: `ca.jamiesinn.prusa-connect-otel`
- Permission: `observe`
- Network access: only the host in `otlp_endpoint`

## Metrics

Every numeric, boolean (1/0) or state value in a printer's state tree becomes
a gauge named after its path. Numeric path segments become attributes:

| State path | Metric | Attributes |
|---|---|---|
| `/tools/0/nozzle_temp` | `prusa.connect.tools.nozzle_temp` (Cel) | `prusa.tools.index=0` |
| `/temperature/bed` | `prusa.connect.temperature.bed` (Cel) | |
| `/tools/0/print_fan_rpm` | `prusa.connect.tools.print_fan_rpm` ({rpm}) | `prusa.tools.index=0` |
| `/job/progress` | `prusa.connect.job.progress` (%) | |
| `/state` | `prusa.connect.state` = 1 | `prusa.state=PRINTING` |

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

| Metric | Type | Attributes |
|---|---|---|
| `prusa.connect.job.settled` | counter | device, `prusa.job.outcome` |
| `prusa.connect.job.duration` | histogram (s) | device, `prusa.job.outcome` |
| `prusa.connect.job.filament_used` | counter (mm) | device |

Prints with no recorded start time are not added to the duration histogram.
Prints that finish while the module isn't running are not exported.

## Configuration

| Key | Default | Description |
|---|---|---|
| `otlp_endpoint` | (required) | `host:port` or URL. A URL path is used as a prefix for `/v1/metrics` and `/v1/traces`. `https://` uses TLS, `http://` doesn't. |
| `otlp_protocol` | `http/protobuf` | or `grpc` (ignores the URL path) |
| `otlp_headers` | | Secret. Comma-separated `key=value` pairs, e.g. `Authorization=Api-Token dt0c01.XXXX` |
| `otlp_insecure` | `false` | Disable TLS for an endpoint without a scheme |
| `otlp_tls_skip_verify` | `false` | Skip certificate verification |
| `service_name` | `prusa-connect-otel` | `service.name` resource attribute |
| `export_interval` | `10s` | Metric export interval |
| `metric_prefix` | `prusa.connect` | Metric name prefix |

Set these on the module's Settings tab under **Settings → Extensions**, or
through the resources API. Don't put them under `resources.sink.otel-exporter`
in `config.yaml`: the module's declaration lives in the server's overlay, and
the server rejects a second one.

```sh
NS=/api/v1/resources/sink/otel-exporter
ETAG=$(curl -s -D - -o decl.json -b cookies "$BASE$NS" | awk -F': ' 'tolower($1)=="etag"{print $2}' | tr -d '\r')
jq '.config = {
      "otlp_endpoint": "https://activegate.example.com:9999/e/<env>/api/v2/otlp",
      "otlp_headers":  "Authorization=Api-Token dt0c01.XXXX"
    }' decl.json > put.json
curl -s -b cookies -X PUT "$BASE$NS" -H 'content-type: application/json' -H "If-Match: $ETAG" --data @put.json
```

The module restarts with the new settings. Until `otlp_endpoint` is set, the
instance fails with `RESOURCE_PLUGIN_SETTINGS`.

For Dynatrace, the API token needs the `metrics.ingest` and `traces.ingest`
scopes.

The manifest allows outbound connections to the `otlp_endpoint` host on ports
443, 4317, 4318 and 9999. If your collector uses another port, add it to
`egress` in `module.yaml`.

## Status

`GET /api/v1/modules/ca.jamiesinn.prusa-connect-otel/status`:

```json
{
  "configured": true,
  "endpoint": "https://activegate.example.com:9999/e/abc/api/v2/otlp",
  "protocol": "http/protobuf",
  "stream": {"ok": true, "last_success": "2026-11-23T21:50:02Z"},
  "last_window": "2026-11-23T21:50:02Z",
  "exports": {
    "metrics": {"ok": false, "last_success": "2026-11-23T21:41:12Z",
                "failing_since": "2026-11-23T21:41:22Z",
                "error": "failed to send metrics to ...: 401 Unauthorized"},
    "traces": {"ok": true, "last_success": "2026-11-23T21:49:30Z"}
  },
  "devices": 3, "values": 51, "metrics": 22, "dropped_values": 0
}
```

An error is shown until the next successful export. The exporter retries 429,
502, 503 and 504 responses for up to a minute before reporting a failure. The
server log gets one line when exports start failing and one when they recover.

## Development

Requires Go 1.26.11. The Prusa Connect SDK isn't published yet, so `go.mod`
points at the `.core` submodule.

```sh
git clone --recurse-submodules https://github.com/JamieSinn/prusa-connect-otel
make check   # gofmt, vet, golangci-lint, go test -race
```

The tests run the module over a real wirerpc session against the SDK's host
handlers, exporting to an in-process OTLP receiver.

To update Prusa Connect, check out the new tag in `.core`, then update
`core-ref` in `.github/workflows/ci.yml` and `release.yml` and `require.core`
in `module.yaml`. CI fails if the submodule and `core-ref` disagree.

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

### Publishing setup

Publishing uses trusted publishing: the marketplace accepts an upload only
with a GitHub OIDC token from this repository, workflow and environment.
Releases are also signed with an RSA key registered with the marketplace.

1. In the repo settings, under Actions → General, allow GitHub Actions to
   create pull requests (needed by release-please).
2. Create a signing key and store the private key in the `release`
   environment, which only `main` can deploy to:

   ```sh
   openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out private_key.pem
   openssl rsa -pubout -in private_key.pem -out public_key.pem
   gh secret set PRUSA_CONNECT_EXTENSION_SIGNING_KEY --env release < private_key.pem
   ```

   Keep a backup of `private_key.pem` in a password manager, then delete it.
   Never commit it.
3. In the [author console](https://plugin-marketplace.prusa3d.com), add
   `public_key.pem` under *Signing keys*, and add a trusted publisher:

   | Field | Value |
   |---|---|
   | Extension id | `ca.jamiesinn.prusa-connect-otel` |
   | Repository | `JamieSinn/prusa-connect-otel` |
   | Workflow | `release.yml` |
   | Environment | `release` |

## License

MIT
