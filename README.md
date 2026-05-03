# Home Net Observer

Home Net Observer is a metadata-only local network monitor for household
networks. It captures packet metadata, summarizes traffic, writes measurements
to InfluxDB, and visualizes the results in Grafana.

The first target setup is a Windows PC with Docker Desktop running the central
InfluxDB/Grafana stack. For full Windows NIC visibility, the collector should
run natively with Npcap. The Docker collector is useful for Linux hosts,
Raspberry Pi, home servers, gateway-style machines, and smoke testing.

## What It Collects

The collector stores metadata only. It does not store packet payloads, website
content, request bodies, cookies, or credentials.

Collected data:

- source and destination IP address
- source and destination MAC address when visible
- reverse DNS hostname when available
- DNS query name and query type when visible
- protocol
- packet counts
- byte counts
- collector name
- timestamps

InfluxDB measurements:

- `device_seen`
- `traffic_flow`
- `dns_query`

## Repository Layout

```text
.
├── collector/                  # Go packet metadata collector
├── dashboards/                 # Grafana dashboard JSON
├── docs/                       # Architecture and roadmap notes
├── provisioning/grafana/       # Grafana datasource and dashboard provisioning
├── docker-compose.yml          # InfluxDB, Grafana, optional collector
├── .env.example                # Example runtime configuration
├── Makefile                    # Repeatable local checks
└── README.md
```

## Requirements

For the central stack:

- Docker Desktop or Docker Engine
- Docker Compose

For native Windows packet capture:

- Go 1.23 or newer
- Npcap installed from `https://npcap.com/`
- A terminal with permission to capture packets

For Linux packet capture:

- Docker Engine or native Go
- `libpcap`
- `NET_RAW`/`NET_ADMIN` capability when running in Docker

## Quick Start

From the project root:

```bash
cd /home/sbera/git/personal/home-net-observer
cp .env.example .env
docker compose up -d influxdb grafana
```

Open Grafana:

```text
http://localhost:3000
```

Default development login from `.env.example`:

```text
admin / admin
```

InfluxDB is available at:

```text
http://localhost:8086
```

Development InfluxDB defaults:

```text
org: home
bucket: network
token: change-me-token
```

Before publishing or using this beyond a local test, change all values in `.env`.

## Run The Collector In Docker

Docker collector mode is best for Linux hosts or for smoke testing. The default
example interface is:

```text
COLLECTOR_INTERFACE=any
```

Start the collector:

```bash
docker compose --profile collector up -d collector
```

Check that it started:

```bash
docker compose logs --tail 50 collector
```

Expected log:

```text
collector started collector=home-pc interface=any influx=http://influxdb:8086
```

List capture interfaces visible inside the collector image:

```bash
docker run --rm home-net-observer-collector:dev -list-interfaces
```

On Docker Desktop for Windows, this container sees Docker networking rather than
the full physical household network. That is expected.

## Run The Collector Natively On Windows

This is the recommended path for testing real traffic from a Windows PC.

1. Start InfluxDB and Grafana with Docker:

   ```powershell
   docker compose up -d influxdb grafana
   ```

2. Install Npcap from:

   ```text
   https://npcap.com/
   ```

3. List interfaces:

   ```powershell
   cd C:\path\to\home-net-observer
   go run .\collector\cmd\collector -list-interfaces
   ```

4. Pick the active Wi-Fi or Ethernet interface name.

5. Run the collector:

   ```powershell
   $env:COLLECTOR_NAME="windows-pc"
   $env:COLLECTOR_INTERFACE="\Device\NPF_{YOUR-INTERFACE-ID}"
   $env:COLLECTOR_INFLUX_URL="http://localhost:8086"
   $env:COLLECTOR_PROMISCUOUS="true"
   $env:INFLUXDB_TOKEN="change-me-token"
   $env:INFLUXDB_ORG="home"
   $env:INFLUXDB_BUCKET="network"
   go run .\collector\cmd\collector
   ```

6. Browse a few sites or run DNS lookups.

7. Refresh Grafana.

## Central Home Server Mode

Run the storage and dashboard stack on one central machine:

```bash
docker compose up -d influxdb grafana
```

On each collector device, point to the central server:

```text
COLLECTOR_INFLUX_URL=http://CENTRAL_SERVER_IP:8086
INFLUXDB_TOKEN=change-me-token
INFLUXDB_ORG=home
INFLUXDB_BUCKET=network
COLLECTOR_NAME=living-room-pc
```

Then run the collector natively or in Docker depending on the host.

## Test Plan

Use these checks in order.

### 1. Validate Compose

```bash
docker compose --env-file .env.example config
```

Expected result: Compose prints the resolved configuration without errors.

Shortcut:

```bash
make compose-config
```

### 2. Build Collector Image

```bash
docker build -f collector/Dockerfile -t home-net-observer-collector:dev .
```

Shortcut:

```bash
make collector-build
```

Expected result: image builds successfully.

### 3. Run Go Tests

The test target installs `libpcap-dev` inside a temporary Go container and runs
the collector tests.

```bash
make collector-test
```

Expected result:

```text
ok  	github.com/sberaconnects/home-net-observer/collector/cmd/collector
```

The current unit test verifies:

- synthetic DNS packets produce `dns_query` metadata
- observed packets produce `device_seen` metadata
- flushed packet summaries produce `traffic_flow` metadata

### 4. Start The Stack

```bash
docker compose up -d influxdb grafana
docker compose ps
```

Expected result:

```text
hno-influxdb   Up   8086
hno-grafana    Up   3000
```

### 5. Check Grafana Provisioning

Inspect Grafana logs:

```bash
docker compose logs --tail 100 grafana
```

Expected useful lines:

```text
inserting datasource from configuration name="Home Network InfluxDB"
finished to provision dashboards
```

Then open:

```text
http://localhost:3000
```

Look for the `Home Net Observer` dashboard under the `Home Network` folder.

### 6. Start Collector

```bash
docker compose --profile collector up -d collector
docker compose logs --tail 50 collector
```

Expected log:

```text
collector started
```

### 7. Confirm InfluxDB Has Data

Query measurement counts:

```bash
docker compose exec -T influxdb influx query \
  --org home \
  --token change-me-token \
  'from(bucket:"network") |> range(start:-10m) |> group(columns:["_measurement"]) |> count()'
```

Expected result after the Docker collector runs:

- `device_seen`
- `traffic_flow`

`dns_query` appears when DNS packets are visible to the capture interface. In
Docker Desktop and some Docker Linux setups, DNS may go through Docker's embedded
resolver path and may not be visible to the collector interface. Native Windows
with Npcap, Linux host capture, or gateway capture is the better DNS test.

### 8. Query A Specific Measurement

Traffic:

```bash
docker compose exec -T influxdb influx query \
  --org home \
  --token change-me-token \
  'from(bucket:"network") |> range(start:-10m) |> filter(fn:(r)=>r._measurement == "traffic_flow") |> limit(n:10)'
```

Devices:

```bash
docker compose exec -T influxdb influx query \
  --org home \
  --token change-me-token \
  'from(bucket:"network") |> range(start:-10m) |> filter(fn:(r)=>r._measurement == "device_seen") |> limit(n:10)'
```

DNS:

```bash
docker compose exec -T influxdb influx query \
  --org home \
  --token change-me-token \
  'from(bucket:"network") |> range(start:-10m) |> filter(fn:(r)=>r._measurement == "dns_query") |> limit(n:10)'
```

## Current Tested Result

The project has been tested locally with Docker.

Passing checks:

- Compose configuration validates.
- InfluxDB starts and initializes org `home` and bucket `network`.
- Grafana starts and provisions the InfluxDB datasource.
- Grafana provisions the `Home Net Observer` dashboard.
- Collector Docker image builds successfully.
- Collector starts with interface `any`.
- Collector writes `device_seen` data to InfluxDB.
- Collector writes `traffic_flow` data to InfluxDB.
- Go unit tests pass for DNS extraction, device metadata, and flow flushing.

Known limitation from this Docker smoke test:

- DNS packets were not visible from the Docker collector interface in this
  environment. This is expected in some Docker setups and should be tested with
  native Windows/Npcap or a host/gateway capture point.

## Common Commands

Start central stack:

```bash
docker compose up -d influxdb grafana
```

Start collector too:

```bash
docker compose --profile collector up -d collector
```

Show status:

```bash
docker compose ps
```

Show logs:

```bash
docker compose logs -f collector
docker compose logs -f grafana
docker compose logs -f influxdb
```

Stop services:

```bash
docker compose --profile collector stop
```

Stop and remove containers while keeping volumes:

```bash
docker compose --profile collector down
```

## Troubleshooting

### Grafana Opens But Dashboard Is Empty

Check whether the collector is running:

```bash
docker compose ps
docker compose logs --tail 50 collector
```

Then query InfluxDB:

```bash
docker compose exec -T influxdb influx query \
  --org home \
  --token change-me-token \
  'from(bucket:"network") |> range(start:-10m) |> group(columns:["_measurement"]) |> count()'
```

### InfluxDB Query Says Unauthorized

Make sure the token matches `.env`:

```text
INFLUXDB_TOKEN=change-me-token
```

If you changed `.env` after the InfluxDB volume was initialized, recreate the
volume or update the token inside InfluxDB.

### Collector Cannot Open Interface

List interfaces:

```bash
docker run --rm home-net-observer-collector:dev -list-interfaces
```

Set `COLLECTOR_INTERFACE` in `.env` to one of the listed names.

For Windows native capture, install Npcap and run the interface listing command
from PowerShell.

### Docker Collector Does Not See Household Traffic

This is normal on Docker Desktop for Windows. The collector container usually
sees the Docker VM network, not the full physical LAN. Use the native Windows
collector with Npcap for real PC-level capture.

For whole-house visibility, run the collector on a gateway, firewall, Linux
bridge, Raspberry Pi bridge, or mirrored switch port.

## Security Notes

- Change `.env` secrets before publishing or long-running use.
- Do not expose InfluxDB or Grafana directly to the internet.
- Treat traffic metadata as sensitive household data.
- Keep metadata-only capture as the default behavior.

## Roadmap

- Native Windows binary packaging.
- Better hostname enrichment from mDNS, DHCP leases, and NetBIOS.
- Device labels.
- Dashboard variables for collector and device.
- Alerts for unusual traffic spikes.
- Optional domain allowlist/denylist dashboards.
- Gateway/Raspberry Pi deployment guide.
