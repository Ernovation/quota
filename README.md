# Quota Server (OpenWrt)

Go web server to manage per-user internet access with per-day-of-week quotas,
carryover caps, and configurable curfew windows.

## Features

- **Per-day-of-week quota** — each user gets a configurable number of minutes
  per weekday (Sunday–Saturday).
- **Carryover** — unused time at end of day rolls over to the next day, capped
  at a per-day configurable maximum.
- **Curfew windows** — the admin can define, per day of the week, a time when
  internet is unconditionally turned off (e.g. `22:00`) and a time from which
  the user is allowed to turn it back on (e.g. `07:00`). The curfew window can
  cross midnight.
- **Admin day-adjustment** — admin can grant or revoke extra minutes for the
  current day (positive or negative value).
- **Automatic shutoff** — internet is disabled automatically when quota is
  exhausted or when a curfew window starts; `off.sh` is called in both cases.
- **External script integration** — `on.sh <username>` / `off.sh <username>`.
- **Built-in web client** — served at `/`, works on desktop and mobile.
- **State persistence** — all data is stored in a single JSON file; existing
  state files with the old single-value quota/cap format are migrated
  automatically on startup.

## API Overview

### Public

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/login` | Authenticate and create a session cookie |
| `POST` | `/api/logout` | Destroy the session cookie |
| `GET`  | `/api/me` | Current user status and quota info |
| `POST` | `/api/me/internet/on` | Turn internet on (blocked during curfew or quota exhausted) |
| `POST` | `/api/me/internet/off` | Turn internet off |

### Admin only

| Method | Path | Description |
|--------|------|-------------|
| `GET`  | `/api/admin/users` | List all users |
| `POST` | `/api/admin/users` | Create a user |
| `PUT`  | `/api/admin/users/{username}/quota` | Set weekly quota schedule |
| `PUT`  | `/api/admin/users/{username}/carryover-cap` | Set weekly carryover cap schedule |
| `PUT`  | `/api/admin/users/{username}/curfew` | Set weekly curfew windows |
| `POST` | `/api/admin/users/{username}/extra-minutes` | Adjust today's minutes (`minutes` may be negative) |
| `PUT`  | `/api/admin/users/{username}/password` | Change password |

### Payload shapes

**Create user** (`POST /api/admin/users`):
```json
{
  "username": "alice",
  "password": "secret",
  "role": "user",
  "weekly_quota_minutes":        [60, 90, 90, 90, 90, 120, 120],
  "weekly_carryover_cap_minutes": [0,  30, 30, 30, 30,  60,  60],
  "weekly_curfew": [
    {"off_time": "21:00", "on_time": "07:00"},
    {"off_time": "22:00", "on_time": "07:00"},
    {"off_time": "22:00", "on_time": "07:00"},
    {"off_time": "22:00", "on_time": "07:00"},
    {"off_time": "22:00", "on_time": "07:00"},
    {"off_time": "23:00", "on_time": "08:00"},
    {"off_time": "22:00", "on_time": "08:00"}
  ]
}
```

Array index order: `0`=Sunday, `1`=Monday, …, `6`=Saturday.

**Set weekly quota** (`PUT …/quota`):
```json
{ "weekly_quota_minutes": [60, 90, 90, 90, 90, 120, 120] }
```

**Set weekly carryover cap** (`PUT …/carryover-cap`):
```json
{ "weekly_carryover_cap_minutes": [0, 30, 30, 30, 30, 60, 60] }
```

**Set curfew** (`PUT …/curfew`):
```json
{
  "weekly_curfew": [
    {"off_time": "22:00", "on_time": "07:00"},
    {"off_time": "22:00", "on_time": "07:00"},
    {"off_time": "22:00", "on_time": "07:00"},
    {"off_time": "22:00", "on_time": "07:00"},
    {"off_time": "22:00", "on_time": "07:00"},
    {"off_time": "23:00", "on_time": "08:00"},
    {"off_time": "",      "on_time": ""}
  ]
}
```

Set `off_time` to `""` (empty string) to disable the curfew for a particular
day. Times are in 24-hour `HH:MM` local time. If `on_time ≤ off_time` the
window is treated as crossing midnight (e.g. `22:00`→`07:00` next morning).

### `GET /api/me` response

```json
{
  "username": "alice",
  "role": "user",
  "weekly_quota_minutes":        [60, 90, 90, 90, 90, 120, 120],
  "weekly_carryover_cap_minutes": [0, 30, 30, 30, 30,  60,  60],
  "weekly_curfew": [ … ],
  "today_quota_minutes":        90,
  "today_carryover_cap_minutes": 30,
  "today_curfew": {"off_time": "22:00", "on_time": "07:00"},
  "in_curfew":   false,
  "carryover_seconds":   1800,
  "extra_seconds_today": 0,
  "total_seconds":       7200,
  "used_seconds":        3000,
  "remaining_seconds":   4200,
  "internet_on": true
}
```

## Configuration

| Environment variable | Default | Description |
|----------------------|---------|-------------|
| `ADDR` | `:8080` | Listen address |
| `STATE_FILE` | `./state.json` | Path to JSON state file |
| `ON_SCRIPT` | `./on.sh` | Script called when internet is enabled |
| `OFF_SCRIPT` | `./off.sh` | Script called when internet is disabled |
| `SESSION_TTL_HOURS` | `24` | Session cookie lifetime in hours |
| `INITIAL_ADMIN_PASSWORD` | `admin` | Password for the auto-created admin account (first run only) |

## First Start

On first run, when no state file exists, an `admin` user is created
automatically with `INITIAL_ADMIN_PASSWORD`. **Change this password
immediately** via the web UI or the API.

## Building on Linux

Requires Go 1.22 or later. Install from <https://go.dev/dl/> or your distro's
package manager.

**Quick build for the current machine:**

```sh
go mod tidy
go build -o quota-server ./cmd/server
./quota-server
```

**Using the build script** (`build.sh`) — produces an optimised, stripped
binary in `dist/<target>/`:

```sh
chmod +x build.sh

# Native Linux (amd64)
./build.sh linux-amd64

# OpenWrt — MIPS big-endian (e.g. many older Atheros-based routers)
./build.sh openwrt-mips

# OpenWrt — MIPS little-endian (e.g. MediaTek MT7621)
./build.sh openwrt-mipsle

# OpenWrt — ARM 64-bit (e.g. Raspberry Pi, some newer routers)
./build.sh openwrt-arm64

# OpenWrt — ARMv7 (e.g. many older ARM-based routers)
./build.sh openwrt-armv7

# Windows (for local testing)
./build.sh windows
```

The optional second argument overrides the output directory:

```sh
./build.sh openwrt-mips /tmp/quota-dist
```

**Running tests:**

```sh
go test ./...
```

## PowerShell Build (Windows)

```powershell
.\build.ps1 -Target windows
.\build.ps1 -Target linux-amd64
.\build.ps1 -Target openwrt-mips
.\build.ps1 -Target openwrt-mipsle
.\build.ps1 -Target openwrt-arm64
.\build.ps1 -Target openwrt-armv7
```

## Web Client

Open `http://<host>:8080/` in a browser after starting the server.

**User view:**
- Internet on/off toggle (disabled automatically during curfew or when quota is
  exhausted; the *Turn Internet On* button is greyed out during a curfew).
- Live display of today's quota, remaining time, carryover, and curfew status.

**Admin panel** (visible when logged in as admin):
- Create users with per-day quota, carryover cap, and curfew schedules.
- Manage existing users — click any row in the users table to pre-fill the
  manage form with that user's current settings.
- Set quota, carryover cap, and curfew independently via dedicated buttons.
- Adjust today's available minutes (positive to grant extra time, negative to
  reduce it).

## Deploying on OpenWrt

1. Cross-compile for your router's architecture (see *Building on Linux* above).
2. Copy the binary and scripts to the router:
   ```sh
   scp dist/openwrt-mips/quota-server root@192.168.1.1:/usr/bin/
   scp on.sh off.sh root@192.168.1.1:/etc/quota/
   ```
3. Make scripts executable:
   ```sh
   ssh root@192.168.1.1 'chmod +x /etc/quota/on.sh /etc/quota/off.sh'
   ```
4. Configure via environment or init script (see `quota-server.init` for an
   OpenWrt procd example):
   ```sh
   cp quota-server.init /etc/init.d/quota-server
   chmod +x /etc/init.d/quota-server
   /etc/init.d/quota-server enable
   /etc/init.d/quota-server start
   ```

## Script Contract

The server calls your scripts with a single argument — the username — using
`exec`. Scripts must be executable and return exit code `0` on success. A
non-zero exit code or any script error is propagated back as an API error and
the state change is **not** committed.

```sh
# on.sh — called when a user's internet is enabled
#!/bin/sh
USERNAME="$1"
iptables -D FORWARD -m owner --uid-owner "$USERNAME" -j DROP 2>/dev/null || true

# off.sh — called when a user's internet is disabled (quota, curfew, or manual)
#!/bin/sh
USERNAME="$1"
iptables -A FORWARD -m owner --uid-owner "$USERNAME" -j DROP
```

## State File Migration

State files created before the per-day-of-week quota update (which used a
single `daily_quota_minutes` and `carryover_cap_minutes` scalar per user) are
detected and automatically migrated on startup: the scalar value is replicated
to all 7 weekday slots and the legacy fields are removed from the file.
No manual intervention is required.
