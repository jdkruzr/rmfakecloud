# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

`rmfakecloud` is a self-hosted replacement for the reMarkable cloud. A single Go binary serves the device-facing sync API, the web UI, and (optionally) MQTT/screenshare. Supports reMarkable 1/2/Paper Pro/Paper Pro Move up to device software 3.27.1. Project documentation lives at https://ddvk.github.io/rmfakecloud/ (sources in `docs/`, served via mkdocs).

## Common commands

Building/running:
- `./dev.sh` — full dev loop: runs `vite` UI dev server (proxied to backend at :3000) and rebuilds/restarts the Go backend on every `*.go` change via `entr`. Requires `entr` installed. Sets dev env: `JWT_SECRET_KEY=dev`, `LOGLEVEL=DEBUG`, dummy SMTP at `localhost:2525`.
- `make build` — builds `dist/rmfakecloud-x64` (depends on UI being built into `ui/dist`).
- `make run` — `go run ./cmd/rmfakecloud` (requires `ui/dist` to exist; the Makefile rule builds the UI first).
- `make runui` — UI-only Vite dev server.
- `make all` — cross-compiles linux x64/armv6/armv7/arm64, windows64, and docker variants.
- `make container` — builds local docker image tagged `rmfakecloud` via `Dockerfile.make` (consumes already-built binary).
- Multi-stage Docker image (production): `docker build -t rmfakecloud .` — builds UI with pnpm then Go binary, output is a scratch image exposing :3000.

Testing/linting:
- `make testgo` or `go test ./...` — run the Go test suite.
- Run a single Go test: `go test ./internal/app -run TestCodeConnector`.
- `make testui` is currently a no-op (`echo "TODO: fix this"`).
- UI lint: `cd ui && pnpm lint` (eslint flat config in `ui/eslint.config.js`).
- There is no Go linter configured in the Makefile; CI (`.github/workflows/go.yml`) only runs `make build`.

CLI (admin) commands — built into the same binary, dispatched in `internal/cli`:
- `rmfakecloud setuser -u <name> [-p <pass>] [-a] [-s]` — create/update user. `-a` makes admin, `-s` enables sync15. Without `-p`, a password is generated and printed.
- See `cli.Usage()` (printed on `-h`) for the complete list. The CLI returns from `main` before `cfg.Verify()` runs, so admin commands don't need a configured server.

## High-level architecture

### Entry point and wiring (`cmd/rmfakecloud/main.go` → `internal/app`)
1. `config.FromEnv()` loads everything from environment variables. There is no config file; `config.EnvVars()` documents them all and is printed on `-h`. Most settings have a `RM_*` or `RMAPI_*` prefix.
2. `cli.New(cfg).Handle(os.Args)` runs first — if argv matches a CLI subcommand it executes and the program exits.
3. Otherwise `app.NewApp` constructs the HTTP server, which is the central composition root that wires together every subsystem.

### `internal/app` — HTTP layer
- `App` struct holds references to every collaborator: `docStorer`, `userStorer`, `metaStorer`, `blobStorer` (storage interfaces), `hub` (websocket notification fanout to devices), `passcodeStore`, `codeConnector` (one-time pairing codes), `hwrClient` (MyScript handwriting recognition), `mqttBroker`, `roomManager` (screenshare).
- `routes.go` defines the full REST surface. The path layout mirrors what the official reMarkable cloud exposes — device firmware is hard-coded to call these paths once it has been redirected via the proxy/cert setup described in the docs.
- Two coexisting sync protocols are served on different paths and selected per-user:
  - **sync10** (the original): document blobs with metadata via legacy endpoints (handled by `docStorer`).
  - **sync15** (diff-based, optional, requires `setuser -s`): blob-addressed storage under `/sync/v2/`, `/sync/v3/`, `/sync/v4/` (handled by `blobStorer` + root indirection). Enabled per-user in the user profile (`sync15: true`).
- Auth uses JWTs signed with `JWT_SECRET_KEY`. Middleware (`middleware.go`) gates routes by claims; the UI uses a different cookie/JWT flow than devices.

### `internal/storage` — persistence
- All persistence is on the local filesystem rooted at `$DATADIR` (default `data/`). There is **no database**.
- Interfaces (`storage/app.go`, `userstorage.go`, `documents.go`, `blobstore.go`, `metadata.go`) abstract storage so it could be swapped, but the only implementation is `storage/fs/` (local files). User profiles are JSON; documents are tar/zip-like bundles for sync10 or content-addressed blobs for sync15.
- `storage/exporter/` renders documents to PDF for the "send by email" feature and the web export.
- **Destructive caveat documented in README:** deleting files from a user's data dir causes the next sync to delete them from the device. Treat `$DATADIR` as authoritative state.

### `internal/ui` — web admin UI backend
- Serves the React app from the embedded `ui/dist` (Go embed via `ui/assets.go`). UI is built separately into `ui/dist` before `go build`.
- Adds its own JSON API under (typically) `/ui/api/...` for the React app, separate from the device API. View models live in `internal/ui/viewmodel/`.

### `ui/` — React frontend
- Vite + React 18 + TypeScript (some files still `.jsx`). Uses pnpm. Bootstrap 5 / react-bootstrap for styling.
- During dev (`vite.config.ts`) proxies API calls to `http://localhost:3000` (the Go backend).
- Production build output (`ui/dist`) is embedded into the Go binary at compile time.

### Supporting subsystems (each in its own `internal/` directory)
- `mqtt/` — embedded MQTT broker (`mochi-mqtt`) for device notifications on newer firmware. Enabled by setting `MQTT_PORT`.
- `screenshare/` — WebSocket-based screen sharing rooms; the device pushes frames, browser clients consume them. Frame assembly handles multi-message + deflate (see recent commit `e70133a`).
- `hwr/` — MyScript handwriting recognition client; only active when `RMAPI_HWR_APPLICATIONKEY` / `RMAPI_HWR_HMAC` are set.
- `integrations/` — cloud storage integrations (WebDAV, FTP working; Dropbox/Google Drive WIP). Pluggable via the `Integration` interface.
- `email/` — SMTP client used by the "send by email" feature, configured via `RM_SMTP_*` envs.
- `messages/` — DTO types shared between handlers and clients.
- `app/hub/` — websocket hub for device push notifications (the legacy non-MQTT path).
- `app/passcodestore/` — stores reset PINs for the passcode-reset flow (rm1/rm2 only).
- `app/codeconnector.go` — the 8-char one-time codes used to pair a device.

### Other `cmd/` binaries
- `cmd/testclient` — small client used to exercise the sync API during development.
- `cmd/history2git15` and `cmd/relinkfile15` — sync15 maintenance utilities operating on the on-disk data directory.

## Things to know before changing code

- The device-facing URL paths are **part of the contract with the device firmware** — they are not free to refactor. Changes there must keep matching what reMarkable's `xochitl` sends.
- Two sync protocols are alive simultaneously. When touching sync logic, check whether a route is sync10 (`docStorer` path) or sync15 (`blobStorer` path under `/sync/v2..v4/`) and which is gated by the user's `sync15` flag.
- The UI is shipped embedded into the Go binary; you must rebuild the UI (`pnpm build` or `make` which depends on `ui/dist`) for UI changes to appear in `make run`/`make build`. `./dev.sh` and `make runui` bypass this by running Vite directly.
- The README warns that breaking changes have shipped at the storage level (notably the v0.0.3 data move and the v0.0.5 sync15 addition). Be cautious with on-disk format changes — there is no migration framework.
- `STORAGE_URL` semantics changed for device SW ≥ 3.15 (it should be unset, or `https://host` without a port). When working on discovery/registration endpoints (`/discovery/v1/*`, `/service/json/1/:service`) keep this in mind.
