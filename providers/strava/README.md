# strava

The [Strava API v3](https://developers.strava.com/docs/reference/): the activity totals of an athlete, rides, runs and swims over the last 4 weeks, the year to date and all time (count, distance, moving and elapsed time, elevation gain), with their longest ride and biggest climb. For your own dashboards: the [API Agreement](https://www.strava.com/legal/api) lets an application show the data of a user to that user only.

It is the first package of the `oauth2_authorization_code` auth type ([ADR 0016](../../docs/adr/0016-oauth2-authorization-code.md)): you accept once in a browser with `tapyr auth login`, then Tapyr renews the tokens by itself.

**Validation status**: simulator only, written from the Strava API documentation (2026-11-27). A report from a real account is welcome.

## Coverage

| Template | Endpoint | Default interval | Requests | Metrics |
|---|---|---|---|---|
| `strava-athlete` | `GET /athlete` | 6h | 1 | `strava_athlete_info` |
| `strava-stats` | `GET /athletes/{id}/stats`, one instance per athlete | 30m | 1 | `strava_activities`, `strava_distance_meters`, `strava_moving_time_seconds`, `strava_elapsed_time_seconds`, `strava_elevation_gain_meters`, `strava_biggest_ride_distance_meters`, `strava_biggest_climb_elevation_gain_meters` |

- `strava-athlete` is the inventory of `strava-stats` (the stats endpoint takes the id of the authenticated athlete): a job using `strava-stats` alone is enough.
- The totals carry `period` (`recent_4w` for the last 4 weeks, `ytd` for the year to date, `all`) and `sport` (`ride`, `run`, `swim`), in meters and seconds as Strava reports them. `ytd` starts over on January 1; `recent_4w` rises with each activity and falls as old ones leave the window.
- Strava counts only the activities visible to Everyone in these totals, and has no totals for other sports (hikes, walks, workouts).

## Auth setup

1. Create an API application at [strava.com/settings/api](https://www.strava.com/settings/api), with `127.0.0.1` as Authorization Callback Domain. A new application is in "Single Player Mode": it can read the data of its owner, which is what this package is for.
2. Give its Client ID and Client Secret to Tapyr, for example `TAPYR_STRAVA_CLIENT_ID` and `TAPYR_STRAVA_CLIENT_SECRET` as in [`examples/strava.yaml`](examples/strava.yaml).
3. Run the consent once, anywhere, and accept in the browser:

   ```sh
   tapyr auth login me --config strava.yaml --secret-file ./strava-refresh-token
   ```

   Paste the address the browser was sent to (the page on `127.0.0.1:8085` may fail to load, only its address matters), or add `--listen` to let Tapyr receive it when the browser runs on the same machine. The refresh token goes to the file, given to the account as `refreshToken: { file: ./strava-refresh-token }`.
4. Start or reload Tapyr. It renews the access token (valid 6 hours) and the refresh token, which Strava replaces at every renewal, and keeps the latest pair in its state store: set `TAPYR_STATE_KEY` to encrypt it. If the access is revoked, the status API tells you to run `tapyr auth login` again.

The package asks for the `read` scope only: the totals need nothing more.

[Configure your accounts](../../docs/configure-accounts.md#accounts-that-need-a-consent-in-a-browser) walks through the rest of a configuration.

## Limits

| Limit | Value | Source |
|---|---|---|
| `read-15min` | 100 read requests every 15 minutes per application, counted over a rolling 15 minutes | [Strava rate limits](https://developers.strava.com/docs/rate-limits/) (checked 2026-11-27) |
| `read-daily` | 1000 read requests per day per application, reset at midnight UTC | [Strava rate limits](https://developers.strava.com/docs/rate-limits/) (checked 2026-11-27) |

- These are the read ("non-upload") limits, lower than the overall ones (200 every 15 minutes, 2000 a day); the package only reads. With upgraded access they double: override `max` in a copy of the package.
- Strava resets the 15-minute limit at 0, 15, 30 and 45 minutes past the hour; Tapyr does not have windows aligned on any duration yet and counts a rolling 15 minutes instead, which never exceeds a fixed window.
- The limits are per application: every account using the same Client ID and Secret shares them, and so does any other tool using your application.
- Strava sends its usage in `X-ReadRateLimit-Usage` as two numbers (15 minutes, day). Tapyr cannot read that format yet, so it does not sync from it; another tool on the same application is seen through its 429s only. Give it its own application.

With the default intervals, one athlete costs 4 athlete requests and 48 stats requests a day, 52 of the 1000; token renewals (every 6 hours) are requests to the OAuth endpoint, not the API.

## Simulator

```sh
tapyr sim --provider strava --listen 127.0.0.1:8081
```

Under `tapyr sim --all --listen-base 127.0.0.1:8081`, it listens on 8089 (`portOffset: 8`). The API is under `/api/v3` and the OAuth endpoints at the root: point an account at it with `baseURL: http://127.0.0.1:8081/api/v3` and `oauthBaseURL: http://127.0.0.1:8081`. It accepts the client `sim-client` / `sim-client-secret`, consents at once, rotates refresh tokens and accepts `sim-refresh-token` as a bootstrap; access tokens last 6 hours. The data is synthetic: one athlete and its totals.

| Scenario | Effect |
|---|---|
| `default` | No fault |
| `quota-tight` | The daily read quota drops to 40 requests, below the 52 of the default intervals: Tapyr stretches the jobs |
| `bad-day` | Another client of the application spends 20 requests an hour; from 2h to 2h20, 30% of the stats requests fail with 503 |

## Dashboard

`dashboards/strava.json` (Grafana, Prometheus data source, athlete variable): the activities, distance, moving time and elevation of the year, distance and moving time per sport, the last 4 weeks per sport, all-time distance, longest ride and biggest climb, and the two read quotas (limit, used by Tapyr and by others).

## Known caveats

- Simulator only so far: the response shapes come from the documentation.
- Only the activities visible to Everyone are counted by Strava's totals.
- No sync from the usage headers (see Limits).
