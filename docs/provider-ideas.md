# Provider ideas

Candidate provider packages, with the rate-limit pattern each one would exercise. Networks are the first use case of Tapyr, not its boundary ([ADR 0011](adr/0011-provider-ecosystem.md)): any metric-oriented API whose quota is worth planning can become a package.

This is a pool of ideas, not a commitment, and contributions are welcome: read [Provider packages](providers.md) first. The limits below were gathered from public documentation in November 2026: **verify them before writing a package**, and cite their sources in the package. A provider that needs one of the [engine gaps](#engine-gaps) starts with an issue, since the engine changes first.

## Network and SD-WAN (first use case)

| Provider | Metrics of interest | Limit pattern | Notes |
|---|---|---|---|
| Juniper Mist | Wi-Fi, switches, SLE (user experience) | 5000 calls/hour per token, reset at the top of the hour | Natural companion to Aruba and Meraki |
| Cisco ThousandEyes | Path latency, loss, availability, app tests | 240 req/min per organization, window starts with the first request; separate per-endpoint limits (instant tests, template deploy); rich `X-Organization-Rate-Limit-*` headers | Needs windows starting at the first request; per-endpoint limits (`appliesTo`) and header sync exist |
| Ubiquiti UniFi Site Manager | Sites, devices, ISP metrics | Verify | Large homelab and SMB audience |
| VeloCloud Orchestrator | SD-WAN links, QoE, failovers | Verify | |
| Peplink InControl2, Cradlepoint NetCloud | 4G/5G routers, signal, data usage | Verify | Same family as Teltonika |
| EfficientIP SOLIDserver, Infoblox | DHCP scope usage, subnet saturation, DNS health | Verify (often on-prem, per-appliance) | High-value, rarely exposed metrics |
| Aruba Central (new, GreenLake) | Same as classic | Verify | Planned separately from `aruba-central-classic` |

## Security and reputation

| Provider | Metrics of interest | Limit pattern | Notes |
|---|---|---|---|
| VirusTotal | Reputation of own IPs/domains | Three simultaneous quotas: per minute, per day (reset 00:00 UTC), per month (reset 00:00 UTC on the 1st); v2 signals exhaustion with 204, v3 with 429 | Multi-window quotas exist; v2 needs rate limiting signaled by a 204 |
| Shodan | Exposure of own IP ranges | Monthly query credits | Needs a request cost (credits) |
| AbuseIPDB | Reports on own IPs | Daily checks quota | |

## Internet measurement

| Provider | Metrics of interest | Limit pattern | Notes |
|---|---|---|---|
| RIPE Atlas | Latency, traceroutes, DNS from probes | Credits per measurement, daily spending cap | Needs a request cost (credits) |
| RIPEstat, PeeringDB, Cloudflare Radar | Prefix visibility, routing, Internet health | Fair-use or per-minute limits, verify | |

## Cloud, costs and AI

| Provider | Metrics of interest | Limit pattern | Notes |
|---|---|---|---|
| AWS Cost Explorer | Spend by service, account, tag | Each request is billed | Needs a request cost, in currency |
| Microsoft Graph, Azure Resource Graph | Intune compliance, Entra sign-ins, resources | Throttling with `Retry-After`, per-tenant and per-app limits | |
| OVHcloud, Scaleway | Consumption, invoices, quotas | Verify | |
| LLM usage APIs (OpenAI, Anthropic admin APIs, LiteLLM) | Tokens, spend per key or team | Verify | Very topical |

## Developer and SaaS

| Provider | Metrics of interest | Limit pattern | Notes |
|---|---|---|---|
| GitHub | Actions runs and durations, PRs, security alerts | REST hourly budget; GraphQL points budget with variable cost per query (returned in the response) | GraphQL needs a request cost settled from the response |
| GitLab | Pipeline durations, runner queues | Per-user and per-IP limits, verify | |
| Okta, Entra ID | Failed sign-ins, MFA | Per-endpoint limits with `X-Rate-Limit-*` headers | |

## Energy, home, sport (homelab showcase)

| Provider | Metrics of interest | Limit pattern | Notes |
|---|---|---|---|
| Enphase | Solar production, consumption | Free plan: 10 hits/min and 1000 hits/month | Tight monthly quota |
| SolarEdge | Solar production | 300 requests/day | Tight daily quota |
| Netatmo | Weather, indoor air quality | Per-user and per-app limits, verify | |
| OpenWeatherMap | Weather | Per-minute and monthly limits depending on plan, verify | |
| Strava | Activities, training load | Read: 100 per 15 min (reset at :00, :15, :30, :45) and 1000 per day (reset at midnight UTC); both values packed in one header (`X-ReadRateLimit-Usage: 15min,daily`) | Needs 15-minute aligned windows and quota sync from expressions |

## Engine gaps

Most of the APIs above fit the configuration model as it is: rate, concurrency, calendar and rolling quota windows, scopes, header sync and `Retry-After` ([`04-budget-engine.md`](spec/04-budget-engine.md)). Five mechanisms are missing. Each one is a generic, configurable feature, designed in its own issue before any package needs it:

| Gap | Today | Needed by |
|---|---|---|
| Request cost | A quota counts one unit per request | GitHub GraphQL (points per query, returned in the response), RIPE Atlas (credits per measurement), AWS Cost Explorer (every request is billed), Shodan (query credits) |
| Quota windows aligned on any duration | A fixed window is a calendar period (minute, hour, day, week, month) | Strava (15 minutes, reset at :00, :15, :30 and :45) |
| Quota sync from expressions | The limit, the remaining and the reset come from one header each | Strava (two windows packed in one header), APIs that report their quota in the body |
| Windows starting at the first request | Fixed windows are aligned on the clock | ThousandEyes (the minute starts with the first request) |
| Rate limiting signaled other than by a status | `errors.rateLimited` matches HTTP statuses | VirusTotal v2 (204 without a body), APIs answering 200 with an error object |

## Sources

- Strava rate limits: https://developers.strava.com/docs/rate-limits/
- VirusTotal consumption quotas: https://docs.virustotal.com/docs/consumption-quotas-handled
- ThousandEyes rate limits: https://developer.cisco.com/docs/thousandeyes/rate-limits/
- Juniper Mist limit (as documented by IP Fabric): https://docs.ipfabric.io/main/IP_Fabric_Settings/Discovery_and_Snapshots/Discovery_Settings/Vendors_API/juniper_mist/
- Enphase developer plans: https://developer-v4.enphase.com/developer-plans
- SolarEdge daily limit (as documented by Home Assistant): https://www.home-assistant.io/integrations/solaredge
