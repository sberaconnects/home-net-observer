# Dockge Deployment

This project can run as a Dockge stack on the home server. The recommended
home-server shape is:

- InfluxDB stores device, traffic, and AdGuard history.
- Web UI shows the household dashboard.
- Collector runs with host networking so it can poll AdGuard, scan the LAN, and
  optionally capture host-visible traffic.

Use `docker-compose.dockge.yml` for Dockge.

## Before Starting

Use `.env.dockge.example` as the Dockge environment template. Copy those values
into Dockge's environment editor and replace the passwords/tokens.

Required values:

```text
STOREPATH=/mnt/store
DOCKER_NETWORK=media_bridge

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

To find the interface on the home server:

```bash
ip -br addr
ip route get 1.1.1.1
```

The interface shown after `dev` in the route command is usually the right one.

The compose file follows the same Dockge pattern as your other services:

- `env_file: .env`
- Watchtower label enabled
- persistent files under `${STOREPATH}/DockerStuff/home-net-observer`
- non-host services attached to external network `${DOCKER_NETWORK}`

Make sure the external Docker network exists:

```bash
docker network ls | grep media_bridge
```

If needed:

```bash
docker network create media_bridge
```

Keep your device labels at:

```text
${STOREPATH}/DockerStuff/home-net-observer/config/devices.csv
```

If you are deploying from Git, copy your private local `config/devices.csv` to
that path on the home server after cloning. Do not commit it.

## Dockge Steps

1. Create a new Dockge stack, for example `home-net-observer`.
2. Use this repository as the stack folder, or paste the contents of
   `docker-compose.dockge.yml` into Dockge.
3. Add the `.env.dockge.example` values in Dockge and replace secrets.
4. Make sure `${STOREPATH}/DockerStuff/home-net-observer/config/devices.csv`
   exists if device labels are enabled.
5. Start the stack.
6. Open the Web UI:

```text
http://HOME_SERVER_IP:8088
```

## First-Run Checks

After the stack starts, check:

```bash
docker logs hno-influxdb --tail 50
docker logs hno-webui --tail 50
docker logs hno-collector --tail 50
```

Expected collector logs:

```text
collector started
adguard collector started
```

Then open:

```text
http://HOME_SERVER_IP:8088/healthz
http://HOME_SERVER_IP:8088
```

If InfluxDB was already initialized with different credentials, changing
`INFLUXDB_TOKEN` in Dockge will not update the existing volume. Either keep the
original token or recreate the InfluxDB volume.

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
- Host networking is used for the collector so LAN scan and host-visible packet
  metadata work better on Linux home servers.
- Treat `.env`, InfluxDB, and AdGuard data as sensitive household data.
