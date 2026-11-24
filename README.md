# OpenTelemetry exporter for Prusa Connect

A [Prusa Connect](https://github.com/prusa3d/prusa-connect) module that exports
the whole fleet's telemetry as OTLP metrics, and every finished print as a span,
to an OpenTelemetry collector or a backend that ingests OTLP (Dynatrace,
Grafana, Honeycomb and others).

It is one Go process the server starts and supervises, and draws nothing in the
app. It reads the server's event stream as itself, so it never polls a printer:
the server already holds every printer's state, and the module exports what the
server publishes.

For a single PrusaLink printer without Prusa Connect, the standalone poller
[otel-prusa-link](https://github.com/JamieSinn/otel-prusa-link) exports the same
kind of telemetry by polling the printer.

| | |
|---|---|
| module path | `github.com/jamiesinn/prusa-connect-otel` |
| namespace | `com.github.aux.jamiesinn.prusa-connect-otel` |
| instance | `sink:otel-exporter` |
| permission | `observe`: read each printer's telemetry, presence, record and finished prints |
| egress | the host in `otlp_endpoint`, and nowhere else |

## What it exports

### Gauges

Every path in a printer's state tree whose value is a number, a boolean (1 or
0) or a state word becomes a gauge. The name is the path under
`metric_prefix`. A numeric segment numbers a member, so it leaves the name and
becomes an attribute named after the segment before it:

| state path | metric | attributes |
|---|---|---|
| `/tools/0/nozzle_temp` | `prusa.connect.tools.nozzle_temp` (Cel) | `prusa.tools.index=0` |
| `/temperature/bed` | `prusa.connect.temperature.bed` (Cel) | |
| `/tools/0/print_fan_rpm` | `prusa.connect.tools.print_fan_rpm` ({rpm}) | `prusa.tools.index=0` |
| `/job/progress` | `prusa.connect.job.progress` (%) | |
| `/state` | `prusa.connect.state`, value 1 | `prusa.state=PRINTING` |

Each data point also carries `prusa.device.id`, and `prusa.device.name`,
`prusa.device.vendor` and `prusa.device.model` when the server knows them.

Which paths exist depends on the printer's driver, so the set is open. A text
value other than `/state` and `/job/state` (a file name, a material) is not
exported, because each value would be a new series. Neither is a path ending in
`id` or `_id`, such as `/job/id`: that number labels something and measures
nothing. Units are guessed from the path (`temp` is Cel, `axis` and `_mm` are
mm, `progress`, `flow` and `speed` are %); a path with no guess has no unit. The module creates at most
256 metric names; past that a new path is dropped and counted on `GET /status`.

The server says when a value changes, not what it is every second. So the
module reads every printer's current state when it starts and whenever the
stream reports a gap, and exports the latest value on every `export_interval`.

`prusa.connect.device.connected` is 1 while the server holds a connection to the
printer and 0 after it goes away. A printer that goes away stops reporting its
other gauges until it comes back, rather than repeating values nobody measured.

### Prints

Each finished print is one `print_job` span, built from the server's
`job.settled` event with the start and end the server recorded. A print that
was running when the module started is still exported when it ends. A failed
print has an error status. Attributes: `prusa.job.id`, `prusa.job.outcome`
(`finished`, `failed`, `cancelled`, `unknown`), `prusa.job.display_name`,
`prusa.job.path`, `prusa.job.machine_state`, `prusa.job.progress`,
`prusa.job.time_printing_s` and `prusa.job.filament_used_mm` when the printer
reported them, and the printer's attributes.

| metric | kind | attributes |
|---|---|---|
| `prusa.connect.job.settled` | counter | printer, `prusa.job.outcome` |
| `prusa.connect.job.duration` | histogram, seconds; a print whose start the server did not record is not timed | printer, `prusa.job.outcome` |
| `prusa.connect.job.filament_used` | counter, mm | printer |

The counters start at zero when the module starts. The event stream is followed
from the moment the module starts, so a print that ended while it was not
running is not exported.

## Configuration

| key | default | |
|---|---|---|
| `otlp_endpoint` | *(required)* | `host:port` or a URL. A root path is kept and `/v1/metrics` and `/v1/traces` are joined onto it. `https://` uses TLS, `http://` does not |
| `otlp_protocol` | `http/protobuf` | or `grpc`, which ignores a root path |
| `otlp_headers` | | a secret: `key=value` pairs, comma separated, such as `Authorization=Api-Token dt0c01.XXXX` |
| `otlp_insecure` | `false` | no TLS for an endpoint with no scheme |
| `otlp_tls_skip_verify` | `false` | accept a certificate that does not verify, such as a self-signed one |
| `service_name` | `prusa-connect-otel` | the `service.name` resource attribute |
| `export_interval` | `10s` | how often the gauges are exported |
| `metric_prefix` | `prusa.connect` | the start of every metric name |

`module add` installs the module into the server's machine-owned overlay, so
its settings are written there too, not in your `config.yaml`. A `config:`
block under `resources.sink.otel-exporter` in `config.yaml` is read as a second
declaration with no `type`, and the server refuses it. Set them one of two ways:

- on the module's page under **Settings → Extensions**, on its Settings tab,
  where `otlp_headers` is masked;
- through the API, which is what that tab calls. Read the declaration, set
  `config`, and write it back with the `ETag` you read as `If-Match`:

```sh
NS=/api/v1/resources/sink/otel-exporter
ETAG=$(curl -s -D - -o decl.json -b cookies "$BASE$NS" | awk -F': ' 'tolower($1)=="etag"{print $2}' | tr -d '\r')
jq '.config = {
      "otlp_endpoint": "https://activegate.example:9999/e/<env>/api/v2/otlp",
      "otlp_headers":  "Authorization=Api-Token dt0c01.XXXX"
    }' decl.json > put.json
curl -s -b cookies -X PUT "$BASE$NS" -H 'content-type: application/json' -H "If-Match: $ETAG" --data @put.json
```

The server restarts the module with the new settings, and keeps them when the
module is installed again over itself. Until `otlp_endpoint` is set the
instance stays failed with `RESOURCE_PLUGIN_SETTINGS`, naming the key.

For Dynatrace the token needs the `metrics.ingest` and `traces.ingest` scopes.

## Status

`GET /api/v1/modules/com.github.aux.jamiesinn.prusa-connect-otel/status`
answers whether the module reads the event stream and whether the latest
export of each signal reached the collector:

```json
{
  "configured": true,
  "endpoint": "https://activegate.example:9999/e/abc/api/v2/otlp",
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

A failure is shown while it is the latest outcome and cleared by the next
success. The OTLP exporter retries a 429, 502, 503 or 504 for up to a minute
before it reports a failure; any other refusal, such as a 401 for a bad token,
is reported on the next export. The server's log carries one line when exports
start failing and one when they recover.

## Build

Go 1.26.11 or newer. The SDK is not published as a Go module yet, so `go.mod`
replaces it with the Prusa Connect checkout in the `.core` submodule:

```sh
git clone --recurse-submodules https://github.com/jamiesinn/prusa-connect-otel
git submodule update --init   # in a clone made without --recurse-submodules
make check                    # gofmt, vet, golangci-lint, tests under -race
```

The submodule's commit and `core-ref` in both workflows name the same Prusa
Connect release, and CI fails when they do not: the marketplace's builder
checks out `core-ref` itself and would otherwise build against another core.
To move to a newer core, check its tag out in `.core`, commit the submodule,
and raise `core-ref` in `.github/workflows/ci.yml` and `release.yml` and
`require.core` in `module.yaml` with it.

### Egress

The manifest declares the host in `otlp_endpoint` on ports 443, 4317, 4318 and
9999. The manifest format pins the ports of an option's host, and 443 is the
default when none are named, so the common OTLP ports are listed. A collector on
another port works (the server does not enforce egress), but the declaration an
operator approves would not name it: add the port to `module.yaml` if you need
one.

## Releasing

Versions and releases are managed by
[release-please](https://github.com/googleapis/release-please) from
[Conventional Commits](https://www.conventionalcommits.org/) on `main`:
`fix:` is a patch, `feat:` a minor version, and a breaking change (`feat!:` or a
`BREAKING CHANGE:` footer) is also a minor version while the version is below 1.0.

1. Every push to `main` updates one open release pull request. It bumps the
   version in `module.yaml`, `main.go` and `openapi.json` (the host refuses to
   start a module whose `plugin.info` disagrees with its manifest;
   `TestTheVersionAgreesEverywhere` holds the three together) and writes
   `CHANGELOG.md`.
2. Merging it tags `v<version>`, creates the GitHub release, and in the same
   run hands that commit to the marketplace's builder
   (`.github/workflows/release.yml`), which builds every platform itself and
   submits the build for review.

The first release is 0.1.0 (`initial-version` in `release-please-config.json`).
To release a particular version, put `Release-As: x.y.z` in a commit's footer.

## Tests

`make test` runs the module the way the server does: over a wirerpc session on
OS pipes, against the SDK's own host-side handlers for `bus.subscribe` and
`device.*`, exporting to an OTLP/HTTP receiver that decodes what arrives. They
cover the gauges and their attributes, a printer going away, a print becoming a
span with the server's timestamps, resuming from the last cursor, a printer the
fleet no longer lists, every refused setting, `GET /status` failing and
recovering, and creating metric names while the reader collects.
