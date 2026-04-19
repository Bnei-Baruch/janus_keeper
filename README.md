# Janus Keeper

Prometheus exporter and watchdog for [Janus WebRTC Gateway](https://janus.conf.meetecho.com/).

Janus Keeper monitors a Janus instance via its Admin and HTTP APIs, exports live metrics to Prometheus, and automatically remounts streaming mountpoints that disappear at runtime.

## Features

- **Prometheus metrics** -- sessions, handles, loaded plugins, streaming mountpoints, and videoroom counts
- **Config drift detection** -- compares on-disk `.jcfg` config files against live Janus state and exposes missing streams/rooms as metrics
- **Auto-remount** -- background loop recreates missing streaming mountpoints with exponential backoff (5 s up to 5 min)
- **Videoroom support** -- optional monitoring of the videoroom plugin alongside streaming

## Metrics

| Metric | Type | Description |
|---|---|---|
| `janus_up` | Gauge | 1 if the last scrape succeeded, 0 otherwise |
| `janus_current_sessions` | Gauge | Number of active Janus sessions |
| `janus_current_handles` | Gauge | Number of open plugin handles |
| `janus_plugin` | Gauge | Loaded plugins (labels: `name`, `state`) |
| `janus_streams_configured` | Gauge | Streams declared in config (label: `plugin`=streaming/videoroom) |
| `janus_streams_mounted` | Gauge | Streams currently mounted/active in Janus (label: `plugin`=streaming/videoroom) |
| `janus_stream` | Gauge | Config entry missing from live Janus (labels: `status`, `id`, `type`, `description`) |

## Installation

```bash
go install github.com/Bnei-Baruch/janus_keeper@latest
```

Or build from source:

```bash
go build -o janus_keeper .
```

## Usage

```bash
janus_keeper \
  --janus.admin-uri="http://127.0.0.1:7608/admingxy" \
  --janus.uri="http://127.0.0.1:7708/janusgxy" \
  --janus.admin-secret-env="JANUS_ADMIN_SECRET" \
  --streaming.config="/etc/janus/janus.plugin.streaming.jcfg"
```

### Flags

| Flag | Default | Description |
|---|---|---|
| `--web.listen-address` | `:9709` | Address to listen on for HTTP and metrics |
| `--web.telemetry-path` | `/metrics` | Path under which to expose metrics |
| `--janus.admin-uri` | `http://127.0.0.1:7608/admingxy` | Janus Admin API URI |
| `--janus.uri` | `http://127.0.0.1:7708/janusgxy` | Janus HTTP API URI |
| `--janus.admin-secret-env` | _(empty)_ | Environment variable containing the admin secret |
| `--janus.timeout` | `5s` | Timeout for Janus API requests |
| `--streaming.config` | `/usr/janusgxy/etc/janus/janus.plugin.streaming.jcfg` | Path to streaming plugin config file |
| `--videoroom.config` | `/usr/janusgxy/etc/janus/janus.plugin.videoroom.jcfg` | Path to videoroom plugin config file |
| `--log.level` | `info` | Log verbosity (`debug`, `info`, `warn`, `error`) |
| `--log.format` | `logfmt` | Log format (`logfmt`, `json`) |

## Running as a systemd service

1. Create the `janus` user that the service runs as:

    ```bash
    sudo useradd --system --no-create-home --shell /usr/sbin/nologin janus
    ```

2. Edit `conf/janus_keeper.service` and replace `CHANGE_ME` with the real Janus admin secret.

3. Install and enable the unit:

    ```bash
    sudo cp conf/janus_keeper.service /usr/lib/systemd/system/janus_keeper.service
    sudo systemctl daemon-reload
    sudo systemctl enable janus_keeper.service
    sudo systemctl start janus_keeper.service
    ```

## How it works

1. **Scrape** -- On each Prometheus scrape, Janus Keeper queries the Admin API for sessions/handles and the HTTP API for server info, streaming mountpoints, and videoroom rooms.
2. **Diff** -- It parses the on-disk `.jcfg` config files and compares them against the live API response. Any stream or room present in config but missing from Janus is reported as a `janus_stream{status="missing"}` metric.
3. **Remount** -- A background goroutine polls every 5 seconds. When it finds a configured streaming mountpoint missing from the live instance, it sends a `create` request to the streaming plugin. Failed attempts back off exponentially up to 5 minutes.

## License

Apache License 2.0 -- see [LICENSE](LICENSE).
