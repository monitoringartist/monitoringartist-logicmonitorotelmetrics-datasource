# Development

## Prerequisites

- Node.js (see `.nvmrc` for version)
- npm
- Go (see `go.mod` for minimum version)
- Mage (for backend builds)
- Docker & Docker Compose (for local Grafana and e2e tests)

## Getting started

```bash
npm install
npm run build
```

## Project structure

| Path              | Description                                                                      |
| ----------------- | -------------------------------------------------------------------------------- |
| `src/`            | Plugin frontend source (webpack-built, bundled into the Grafana plugin zip)      |
| `pkg/datasource/` | Plugin Go backend entrypoint wiring `promlib` with the LogicMonitor HTTP adapter |
| `pkg/lm/`         | LogicMonitor authentication, token management, and Prometheus-to-LM URL mapping  |
| `pkg/lmadapter/`  | HTTP middleware adapting Prometheus API requests/responses for LogicMonitor      |
| `pkg/schema/`     | `dsconfig` configuration schema — single source of truth for settings            |
| `tests/lm-mock/`  | Local LogicMonitor OTel metrics mock server (`npm run server:mock`)              |
| `.config/`        | Grafana plugin tooling config — **do not modify**                                |

## Running locally

Build the plugin, then start Grafana with the default environment:

```bash
mage -v buildLocal
npm run build
docker compose up -d
```

The default environment starts Grafana used by the e2e tests.

| Command               | Additional data                                                      |
| --------------------- | -------------------------------------------------------------------- |
| `npm run server`      | Grafana with provisioned datasources (no mock API)                   |
| `npm run server:mock` | Grafana + local LogicMonitor OTel Metrics API mock (`tests/lm-mock`) |

### Local mock API (`acme` account)

For local development without a real LogicMonitor portal, when `lm_mock_base_url` is set under
`[plugin.monitoringartist-logicmonitorotelmetrics-datasource]` in `grafana.ini` (configured automatically
in `docker-compose.mock.yaml`, which Grafana passes to the plugin as `GF_PLUGIN_LM_MOCK_BASE_URL`),
`Config.BaseURL()` in `pkg/lm/config.go` routes the placeholder `acme` account to `http://localhost:8080`
instead of `https://acme.logicmonitor.com`. This lets the provisioned `acme` datasources in
`provisioning/datasources/datasources.yml` work out of the box with the mock server in `tests/lm-mock`:

- Start Grafana together with `lm-mock` via `npm run server:mock` (uses `docker-compose.mock.yaml`), or run
  `go run ./tests/lm-mock` directly on the host with `lm_mock_base_url = http://localhost:8080` under
  `[plugin.monitoringartist-logicmonitorotelmetrics-datasource]` in `conf/custom.ini`.
- `lm-mock` serves predictable, time-varying sample OpenTelemetry metrics (`mocked_system_cpu_time`,
  `mocked_system_memory_usage`, `mocked_http_server_request_duration_seconds_count`,
  `mocked_http_server_active_requests`) as well as a `mocked_sinus` metric with two `service_instance_id` series
  (`sinus-instance-1`, `sinus-instance-2`) at different frequencies and phases for visual graph testing.

The scripts are shorthand for layering one override onto the base file. The base
`docker-compose.yaml` stays at the repo root.

Stop the active environment before selecting another one:

```bash
docker compose down --remove-orphans
```

For starting with a specific Grafana version

```bash
GRAFANA_VERSION=13.0.1 docker compose up
```

Grafana will be available at `http://localhost:3000`.

## Running locally against a local grafana/grafana checkout

If you want to develop against a local build of Grafana itself rather than the Docker image, follow these steps.

### 1. Set up the Grafana repository

Clone [grafana/grafana](https://github.com/grafana/grafana) somewhere in your workspace, for example next to this repository:

```
workspace/
  grafana/          ← grafana/grafana checkout
  plugins/
    monitoringartist-logicmonitorotelmetrics-datasource/   ← this repo
```

### 2. Create a `custom.ini`

Grafana's [defaults.ini](https://github.com/grafana/grafana/blob/main/conf/defaults.ini#L25-L26) looks for additional plugins in `data/plugins`. It is cleaner to keep your plugin repos in a dedicated directory (e.g. `workspace/plugins`) and point Grafana there with a `custom.ini` file instead of touching `defaults.ini`.

Create `conf/custom.ini` next to `conf/defaults.ini` in the grafana/grafana repo and add at minimum:

```ini
app_mode = development
force_migration = true

[paths]
plugins = /your/workspace/plugins

[plugins]
allow_loading_unsigned_plugins = monitoringartist-logicmonitorotelmetrics-datasource

# Optional: route the placeholder "acme" account to a local tests/lm-mock server
# [plugin.monitoringartist-logicmonitorotelmetrics-datasource]
# lm_mock_base_url = http://localhost:8080

[log]
level = debug
```

### 3. Start Grafana

Start grafana/grafana with `npm ci && npm run start` for the frontend in one terminal and `make run` for the backend in another. Grafana will use
the plugin from `workspace/plugins/monitoringartist-logicmonitorotelmetrics-datasource`, and you can iterate on frontend or backend changes directly.

### 4. Build the plugin

**Backend** — build the Go binary for your local OS/architecture (or run `mage` to build all platforms):

```bash
mage -v buildLocal
```

You must re-run this command after every backend change. After rebuilding, tell Grafana to reload the plugin:

```bash
mage reloadPlugin
```

**Frontend** — install dependencies and start the watch mode:

```bash
npm install
npm run dev
```

`npm run dev` starts an incremental build that picks up frontend changes automatically. This is powered by [`@grafana/create-plugin`](https://www.npmjs.com/package/@grafana/create-plugin), the base scaffolding tool used for Grafana plugins.

---

## Testing

```bash
npm run test:ci          # unit tests (plugin + scripts)
npm run e2e              # playwright e2e tests (requires running Grafana)
npm run lint             # eslint
npm run typecheck        # typescript type checking
mage test                # go backend tests (includes the schema conformance suite)
go test -race ./pkg/...  # go backend tests with race detector
```

---

## Data source configuration schema

`pkg/schema/dsconfig.json` is the single source of truth for the data source's settings.
After changing it — or after updating `settingsJSONModel` in
`pkg/schema/dsconfig_test.go` — regenerate and verify the committed artifacts:

```bash
go generate ./pkg/schema/...   # rewrite the *.gen.json artifacts
go test ./pkg/schema/...       # conformance suite (drift, validity, struct parity)
```

See [Data Source Configuration Schema](CONTRIBUTING.md#data-source-configuration-schema)
in the contributing guide for the walkthrough of adding a new settings option, and the
[`grafana/dsconfig` docs](https://github.com/grafana/dsconfig/tree/main/dsconfig) for the
schema format itself.
