# Contributing to LogicMonitor OTel Metrics Data Source for Grafana

## Signed commits are required

> [!IMPORTANT]
> All commits must be [signed](https://docs.github.com/en/authentication/managing-commit-signature-verification/signing-commits) (GPG, SSH, or S/MIME)
> to be merged into this repository. Pull requests with unsigned commits will need to be re-committed with signatures before they can be merged.

## Versioning

> [!IMPORTANT]
> Version bumps happen **only** in dedicated release commits/PRs by updating `version` in `package.json` and `CHANGELOG.md`, then pushing a `v*` tag (for example `v13.2.7`).
>
> In normal feature or bug-fix PRs, do not bump `version` in `package.json`.

Thank you for your interest in contributing! This guide covers how to participate in this open-source project.

You can browse [existing issues](https://github.com/monitoringartist/monitoringartist-logicmonitorotelmetrics-datasource/issues) or open a new one before submitting a pull request —
especially for larger changes, it's worth discussing the approach first.

## Required Tools

| Tool                              | Notes                                       |
| --------------------------------- | ------------------------------------------- |
| [Git](https://git-scm.com/)       | Version control                             |
| [Go](https://go.dev/)             | See `go.mod` for minimum version            |
| [Mage](https://magefile.org/)     | Backend build tool                          |
| [Node.js](https://nodejs.org/)    | `>=24`; see `.nvmrc` for the pinned version |
| [npm](https://www.npmjs.com/)     | JavaScript package manager                  |
| [Docker](https://www.docker.com/) | Required for local Grafana and e2e tests    |

### Package manager version

This repository defines the required package manager and its exact version in the
`packageManager` field of `package.json`. You don't have to use it, but enabling
[Corepack](https://github.com/nodejs/corepack) is a convenient way to make your
terminal automatically use that version instead of whatever `npm` you have
installed globally:

Corepack is included with many Node.js distributions. Check whether it is
available:

```bash
corepack --version
```

If the command is unavailable, install the standalone Corepack package:

```bash
npm install --global --ignore-scripts corepack
```

Then enable its npm shim:

```bash
corepack enable npm
```

Restart your terminal after enabling Corepack. No directory-change hook is
required: once enabled, Corepack reads the nearest `package.json` whenever you
run `npm`, in any directory. You can verify the selected version from the
repository directory:

```bash
npm --version
```

Corepack manages the package manager version only; it does not install or select
the Node.js version specified by the `engines` field.

## Frontend Development

Install dependencies:

```bash
npm install
```

Build the plugin frontend (one-shot):

```bash
npm run build
```

Watch mode (rebuilds on file change):

```bash
npm run dev
```

Run frontend unit tests:

```bash
npm test         # interactive watch mode
npm run test:ci  # single-run, used in CI
```

Type-checking:

```bash
npm run typecheck
```

Lint:

```bash
npm run lint
npm run lint:fix
```

## Backend Development

Build the backend binary with Mage. There's no target for just your desktop
platform (only the exotic `LinuxS390X`/`WindowsARM64` targets exist
standalone), so this builds all of them:

```bash
mage
```

Run backend tests:

```bash
mage test          # go test ./pkg/...
go test -race ./pkg/...
```

## Data Source Configuration Schema

`pkg/schema/dsconfig.json` is the **single source of truth** for the data source's
configuration surface — every field a user can set, where it is stored (`root`,
`jsonData`, `secureJsonData`), its type, validation rules and UI hints. It is consumed by
provisioning tooling, documentation and automation.

The schema format is defined and documented by [`grafana/dsconfig`](https://github.com/grafana/dsconfig/tree/main/dsconfig):

- [README](https://github.com/grafana/dsconfig/tree/main/dsconfig#readme) — concepts and a worked example for each field shape (root / jsonData /
  secret / array / virtual), plus current gaps and limitations.
- [`schema.md`](https://github.com/grafana/dsconfig/blob/main/dsconfig/schema.md) — full property reference.
- [`schema.json`](https://github.com/grafana/dsconfig/blob/main/dsconfig/schema.json) — the JSON Schema `dsconfig.json` validates against. It is
  pinned via the `$schema` key at the top of our file, so editors autocomplete from it; bump that URL when you bump
  `github.com/grafana/dsconfig/schema` in `go.mod`.

The rest of this section covers only what is specific to this repository.

### Layout

| File in `pkg/schema/` | Description                                                                                                                       |
| --------------------- | --------------------------------------------------------------------------------------------------------------------------------- |
| `dsconfig.json`       | Source of truth — **edit this**                                                                                                   |
| `dsconfig_test.go`    | Wires the schema into the shared conformance suite; also holds `SecureKeys` and the provisioning examples shipped with the plugin |
| `*.gen.json`          | Generated artifacts — **never hand-edit**; `npm run build` copies them into `dist/schema/` via `webpack.config.ts`                |

### Adding a new settings option

1. **Declare the field** in `pkg/schema/dsconfig.json` under `fields`, and add its `id` to
   the appropriate `groups[].fieldRefs` entry. Field ids follow the `<target>_<key>`
   convention, e.g. `jsonData_httpMethod`.
2. **Add the matching Go field** to `settingsJSONModel` in `pkg/schema/dsconfig_test.go` with
   a json tag equal to the schema `key`. This parity is enforced in both directions — a
   field in the schema but not the struct (or vice versa) fails the test suite. Secrets (`target: secureJsonData`) are the exception: they get no
   struct field, but their key
   must be added to `SecureKeys` in `pkg/schema/dsconfig_test.go`.
3. **Regenerate the artifacts** and commit them with your change:

   ```bash
   go generate ./pkg/schema/...
   ```

4. **Verify**:

   ```bash
   go test ./pkg/schema/...
   ```

If you add a setting that changes what a typical configuration looks like, update
`SettingsExamples` in `pkg/schema/dsconfig_test.go` too — those are the provisioning
payloads shipped with the plugin. Use placeholders like `REPLACE_WITH_PASSWORD`, never
real credentials.

### When the conformance suite fails

Most failures are self-explanatory from the assertion message. The three you are most
likely to hit:

- `SchemaArtifactInSync` — a `.gen.json` file has drifted. Run `go generate ./pkg/schema/...` and commit the result.
- `JSONDataMatchesStruct` / `JSONDataTypesMatchStruct` — the schema and `PromOptions` disagree on keys or types. Update whichever side is behind.
- `SecureValuesMatchLoadSettings` — the schema's `secureJsonData` fields and `SecureKeys` disagree.

## Running Locally

Start a local Grafana instance with the plugin pre-loaded:

```bash
docker compose up -d
```

The default Compose stack is also the e2e stack. To run Grafana with the local LogicMonitor OTel Metrics mock API (`tests/lm-mock`), use:

```bash
npm run server:mock
```

See [DEVELOPMENT.md](./DEVELOPMENT.md#running-locally) for the services,
representative metrics, and shutdown commands for each environment.

For starting with a specific Grafana version

```bash
GRAFANA_VERSION=13.0.1 docker compose up
```

Grafana will be available at `http://localhost:3000` (default credentials: `admin` / `admin`).

## End-to-End Tests

E2E tests use [Playwright](https://playwright.dev/) via `@grafana/plugin-e2e`. Start the server first, then run the tests:

```bash
npm run server   # starts Grafana via Docker
npm run e2e
```

## Pull Requests

- Keep PRs focused — one logical change per PR.
- Add or update tests for any changed behaviour.
- Ensure `npm run lint`, `npm run typecheck`, and `npm run test:ci` all pass locally before opening a PR.
- If you touched data source settings, run `go generate ./pkg/schema/...` and commit the regenerated `.gen.json` artifacts.

## Release Process

> Releases require repository commit access. The steps below are for maintainers.

1. Update `version` in `package.json` (e.g. `npm version patch --no-git-tag-version`) and update `CHANGELOG.md` if applicable.
2. Commit and merge the release PR to `main`.
3. Create and push a `v*` tag (for example `git tag v13.2.7 && git push origin v13.2.7`).
   The [Release workflow](.github/workflows/release.yaml) builds, signs, attests, and drafts a GitHub Release with the plugin archive.
