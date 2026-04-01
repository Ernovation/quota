# Quota Server (OpenWrt)

Go web server to manage user internet access with daily quotas.

Features:
- User and admin login with password
- Admin-managed daily quota per user
- Admin can adjust one-day minutes per user (positive or negative)
- Per-user carryover cap for unused time to next day
- User can turn internet on and off
- Automatic shutoff when daily quota is exhausted
- External script integration (`on.sh` and `off.sh`)
- Built-in web client at `/`

## API Overview

- `POST /api/login`
- `POST /api/logout`
- `GET /api/me`
- `POST /api/me/internet/on`
- `POST /api/me/internet/off`
- `GET /api/admin/users` (admin)
- `POST /api/admin/users` (admin)
- `PUT /api/admin/users/{username}/quota` (admin)
- `PUT /api/admin/users/{username}/carryover-cap` (admin)
- `POST /api/admin/users/{username}/extra-minutes` (admin, `minutes` may be negative)
- `PUT /api/admin/users/{username}/password` (admin)

## Configuration

Environment variables:
- `ADDR` (default `:8080`)
- `STATE_FILE` (default `./state.json`)
- `ON_SCRIPT` (default `./on.sh`)
- `OFF_SCRIPT` (default `./off.sh`)
- `SESSION_TTL_HOURS` (default `24`)
- `INITIAL_ADMIN_PASSWORD` (default `admin`)

## First Start

On first run, if state file does not exist, an `admin` user is created.
Change its password immediately.

## Build

```sh
go mod tidy
go build -o quota-server ./cmd/server
```

Build with script (PowerShell):

```powershell
.\build.ps1 -Target windows
.\build.ps1 -Target linux-amd64
.\build.ps1 -Target openwrt-mips
.\build.ps1 -Target openwrt-mipsle
.\build.ps1 -Target openwrt-arm64
```

## Web Client

After starting the server, open:

- `http://<router-ip>:8080/`

The UI supports:
- Login/logout
- User internet on/off actions
- Live quota/remaining display
- Admin user creation
- Admin quota/carryover/day-adjustment and password updates

## OpenWrt Build Notes

For OpenWrt, cross-compile for your router CPU. Examples:

```sh
# mips
set GOOS=linux
set GOARCH=mips
set GOMIPS=softfloat
go build -o quota-server ./cmd/server

# arm64
go env -w GOOS=linux GOARCH=arm64
go build -o quota-server ./cmd/server
```

On the router:
1. Copy `quota-server`, `on.sh`, `off.sh`.
2. Make scripts executable: `chmod +x on.sh off.sh`.
3. Start server: `./quota-server`.

## Script Contract

The server executes:
- `on.sh <username>` when internet is enabled
- `off.sh <username>` when internet is disabled

Scripts should return exit code `0` on success.
