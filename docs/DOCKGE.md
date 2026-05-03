# Dockge Deployment

This project can run as a Dockge stack on the home server. The recommended
home-server shape is:

- InfluxDB stores device, traffic, and AdGuard history.
- Web UI shows the household dashboard.
- Collector runs with host networking so it can poll AdGuard, scan the LAN, and
  optionally capture host-visible traffic.

Use `docker-compose.dockge.yml` for Dockge.

## Before Starting

Create or update `.env` beside the compose file. Required values:

```text
INFLUXDB_USERNAME=admin
INFLUXDB_PASSWORD=change-this
INFLUXDB_ORG=home
INFLUXDB_BUCKET=network
INFLUXDB_TOKEN=change-this-token

WEBUI_HTTP_PORT=8088
WEBUI_LAN_PREFIX=192.168.178.

COLLECTOR_NAME=home-server
COLLECTOR_PACKET_ENABLED=true
COLLECTOR_INTERFACE=eth0
COLLECTOR_PROMISCUOUS=true
COLLECTOR_FLUSH_INTERVAL=10s
COLLECTOR_LOG_LEVEL=info
COLLECTOR_SCAN_ENABLED=true
COLLECTOR_SCAN_INTERVAL=5m
COLLECTOR_SCAN_CIDR=192.168.178.0/24
COLLECTOR_SCAN_MAX_HOSTS=1024

ADGUARD_BASE_URL=http://192.168.178.61
ADGUARD_USERNAME=your-adguard-user
ADGUARD_PASSWORD=your-adguard-password
ADGUARD_ENABLED=true
ADGUARD_POLL_INTERVAL=30s
ADGUARD_QUERY_LIMIT=500
```

Set `COLLECTOR_INTERFACE` to the real interface on the home server, for example
`eth0`, `enp3s0`, or `wlan0`.

Keep `config/devices.csv` beside the compose file if you want friendly names.

## Dockge Steps

1. Create a new Dockge stack, for example `home-net-observer`.
2. Point it at this project folder, or paste the contents of
   `docker-compose.dockge.yml`.
3. Add the `.env` values in Dockge.
4. Make sure `config/devices.csv` exists if device labels are enabled.
5. Start the stack.
6. Open the Web UI:

```text
http://HOME_SERVER_IP:8088
```

## What Website Detail Is Available

The Web UI can show DNS-level website activity from AdGuard:

- device/client IP
- domain requested
- allowed or blocked status
- block reason and rule when AdGuard blocks it
- timestamp
- query volume per device and domain
- upstream DNS resolver used for allowed requests
- per-device search and allowed/blocked filters

This is enough for household monitoring and policy enforcement. It does not show
full HTTPS URLs, page contents, messages, videos watched, or app payloads.

## Web UI Features

The Web UI is the main day-to-day view. It includes:

- household device inventory with friendly labels
- traffic comparison by device
- `Needs Review` insights for unknown devices, most-blocked devices, noisy
  domains, and data freshness
- `Quick Find` search for devices, IP addresses, and domains
- per-device profile pages with DNS query count, blocked count, block rate, and
  observed traffic
- per-device website activity with allowed/blocked status, AdGuard reason,
  rule/upstream, and filters

## How A Request Is Processed

The data path is:

1. A device asks DNS for a domain.
2. The router/device sends DNS to AdGuard Home.
3. AdGuard allows or blocks the domain using its rules.
4. The collector polls AdGuard query logs.
5. The collector writes `adguard_query` points to InfluxDB.
6. The Web UI reads InfluxDB and shows household/device statistics.

Blocking should happen in AdGuard. Home Net Observer should explain and surface
what happened, then later it can add buttons that call AdGuard APIs for approved
block/unblock workflows.

## Notes

- For reliable per-device website/domain visibility, all household devices must
  use AdGuard as DNS and AdGuard must see real client IPs, not only the router IP.
- Packet capture from a Docker container only sees traffic visible to that host.
  For whole-house packet visibility, run on a gateway, bridge, mirrored switch
  port, or rely mainly on AdGuard DNS logs.
- Treat `.env`, InfluxDB, and AdGuard data as sensitive household data.
