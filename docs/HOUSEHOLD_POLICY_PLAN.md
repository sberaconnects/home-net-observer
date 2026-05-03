# Household Monitoring And Blocking Plan

This project should run as a local-only household visibility and policy stack.
The goal is to monitor all devices that use the home network, identify risky or
mistaken domains, and block them through AdGuard Home.

## Goals

- Monitor PCs, phones, TVs, consoles, and tablets on the household network.
- Keep visibility device-focused: who queried what domain and when.
- Detect online games, chat services, adult content, malware, ads, trackers, and
  manually blocked domains.
- Block unwanted domains through AdGuard Home.
- Show history in Grafana by device, category, domain, and blocked/allowed
  status.
- Run the stack on the home server in Docker/Dockge.

## Non-Goals

- Do not capture private message contents.
- Do not capture HTTPS payloads, passwords, cookies, or browsing page content.
- Do not perform man-in-the-middle TLS inspection.
- Do not expose Grafana, InfluxDB, or AdGuard admin UI to the internet.

## Recommended Architecture

```text
All household devices
        |
        | DNS queries
        v
AdGuard Home on home server
        |
        | query log API / block status / client identity
        v
Home Net Observer collector
        |
        | time-series events
        v
InfluxDB
        |
        v
Grafana dashboards
```

AdGuard Home is the enforcement point. Home Net Observer should not try to block
traffic itself. It should ingest AdGuard query logs, enrich device names, and
build dashboards.

## Current AdGuard Home Instance

The local AdGuard Home admin UI/API base URL is:

```text
http://192.168.178.61
```

The browser URL may show `http://192.168.178.61/#`, but the `#` fragment is only
used by the web UI. API calls use paths below `/control`, for example:

```text
http://192.168.178.61/control/status
http://192.168.178.61/control/querylog
```

The API requires authentication. Configure:

```text
ADGUARD_BASE_URL=http://192.168.178.61
ADGUARD_USERNAME=...
ADGUARD_PASSWORD=...
```

Do not commit real AdGuard credentials.

## Network Setup

All clients must use AdGuard Home for DNS. The best setup is usually:

1. Home server runs AdGuard Home.
2. Router DHCP advertises the AdGuard server IP as the only DNS server.
3. Router blocks or redirects outbound DNS traffic to any other DNS server.
4. AdGuard Home identifies clients by IP, hostname, and configured client names.

Without router/DHCP control, some phones, TVs, browsers, and apps may bypass the
local DNS server.

## Blocking Scope

AdGuard Home can block at DNS level:

- domains
- wildcard domains
- blocklists
- parental/security categories supported by AdGuard Home
- services represented by known domain lists

DNS blocking cannot reliably see or block:

- content inside encrypted HTTPS traffic
- messages inside chat apps
- exact pages on a site when the domain is allowed
- VPN traffic
- DNS-over-HTTPS or DNS-over-TLS if a client bypasses the local resolver

## Teen Usage Monitoring

Recommended categories:

- gaming platforms
- chat and messaging platforms
- adult and explicit content
- malware and phishing
- trackers and ads
- newly observed domains
- manually blocked domains

Recommended dashboards:

- device activity timeline
- top queried domains by device
- blocked domains by device
- gaming/chat category activity by time of day
- first-seen domains
- repeated blocked attempts
- devices bypassing AdGuard or not using DNS

## Data Model

Add these InfluxDB measurements:

- `adguard_query`
- `adguard_block`
- `device_identity`
- `domain_category`

Recommended tags:

- `client_ip`
- `client_name`
- `device_name`
- `device_kind`
- `domain`
- `query_type`
- `status`
- `reason`
- `rule`
- `service`
- `category`

Recommended fields:

- `count`
- `elapsed_ms`
- `blocked`

## Implementation Phases

### Phase 1: AdGuard Home Ingestion

- Add an `adguard` collector mode.
- Read AdGuard Home query log through its local API:
  `GET /control/querylog`.
- Write allowed and blocked DNS queries to InfluxDB.
- Add Grafana panels for top domains, blocked domains, and per-device activity.

Current implementation:

- Polls `GET /control/querylog`.
- Writes `adguard_query` points to InfluxDB.
- Tracks client IP, client name, domain, query type, status, reason, rule,
  upstream, blocked status, answer count, and elapsed time.
- Adds overview dashboard panels for DNS queries and blocked domains.

### Phase 2: Blocking Workflows

- Add config files for custom block categories.
- Add documentation for AdGuard custom filtering rules.
- Add dashboards that show domains worth reviewing.
- Add a safe manual workflow: review first, then block in AdGuard.

### Phase 3: Device Identity

- Use AdGuard client names.
- Keep manual MAC/IP labels.
- Add router DHCP lease import where available.
- Show device owner/category labels in Grafana.

### Phase 4: Bypass Detection

- Detect devices seen in packet/ARP scan but absent from AdGuard logs.
- Document router firewall rules to prevent DNS bypass.
- Show possible bypass devices in Grafana.

## Dockge Deployment

For Dockge, run this as a single Compose stack on the home server:

- `adguardhome`
- `influxdb`
- `grafana`
- `home-net-observer`

AdGuard Home needs port 53 on the LAN. If another DNS service already uses port
53, resolve that conflict first.

## Privacy And Household Safety

This system should be used transparently and proportionately. DNS monitoring is
powerful household metadata. Keep access limited, retain only what is needed,
and avoid capturing private content.
