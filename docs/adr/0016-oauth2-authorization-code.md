# ADR 0016 - OAuth 2.0 authorization code, bootstrapped by tapyr auth login

- Status: accepted
- Date: 2026-11-26

## Context

Many metric-oriented APIs outside networking authenticate with the OAuth 2.0 authorization code flow ([RFC 6749, section 4.1](https://www.rfc-editor.org/rfc/rfc6749#section-4.1)): a person consents once in a browser, the client receives an access token and a refresh token, and lives on refresh tokens afterwards. Strava, Netatmo, Enphase and most consumer and SaaS APIs work this way. None of the declarative auth types of [05](../spec/05-collection.md#declarative-types) covers it: `oauth2_client_credentials` has no person in the loop, and `session_login` posts a password.

Tapyr already runs one such flow, in Go: the `aruba_central_classic` plugin renews access tokens with **rotating** refresh tokens (every refresh returns a new refresh token and invalidates the previous one). Its rules were verified against the real platform: start from a refresh token given as a secret, refresh before expiry, write the new pair to the state store **before** using it, serialize refreshes, let a changed bootstrap secret (detected by its hash) replace the stored pair, and report the re-bootstrap condition (`tapyr_auth_reauth_required`, `/readyz`, the status API). What it lacks for other APIs is a way to obtain the first refresh token: Aruba Central generates one in its UI, other vendors only issue it at the end of a consent in a browser.

"Declarative first" asks that a package for such an API be YAML only. The roadmap puts this in v0.8 ([roadmap](../roadmap.md#v08-more-apis)).

Constraints:

- Tapyr usually runs on a server, a container or a Kubernetes pod, with no browser, and often where no person can type.
- `tapyr run` never prompts.
- Every outbound request, token requests included, goes through a budget reservation.
- Secrets never appear in logs, errors, the status API, the UI or recordings.
- The status API and the UI are read-only.

## Decision

1. **A declarative auth type, `oauth2_authorization_code`.**

   ```yaml
   auth:
     type: oauth2_authorization_code
     oauth2AuthorizationCode:
       authorizeURL: https://www.example.com/oauth/authorize
       tokenURL: https://www.example.com/oauth/token
       clientId: { secret: clientId }
       clientSecret: { secret: clientSecret }   # optional: a public client relies on PKCE
       refreshToken: { secret: refreshToken }   # the bootstrap, written by tapyr auth login
       scopes: [read, activity:read_all]
       scopeSeparator: ","                     # default a space (RFC 6749); some APIs want commas
       redirectURL: '{{ .params.redirectURL | default "http://127.0.0.1:8085/callback" }}'
       pkce: S256                               # S256 (default) or none
       authStyle: body                          # client credentials in the header (default) or the body
       authorizeParams: { approval_prompt: force }   # optional, added to the authorize URL
       tokenParams: {}                          # optional, added to every token request
       deviceAuthorizationURL: https://www.example.com/oauth/device   # optional, enables --device
       refreshBefore: 5m                        # default 5m
   ```

   The URLs are templates, as `tokenURL` of `oauth2_client_credentials`, so that a package can point them at a simulator. The access token is sent as `Authorization: Bearer <token>`; an `apply` block (the `TokenApply` of `session_login`) can change that for an API that wants another header. Token responses are read with the standard fields (`access_token`, `refresh_token`, `expires_in`); an access token without `expires_in` is used until it is rejected.

   The token belongs to the account: a target cannot override `clientId`, `clientSecret` or `refreshToken` (validation rejects it). An API where each person consents for their own data (an athlete, a household member) gets one account per person, each with its own login.

2. **`tapyr run` behaves like the Aruba plugin, generalized.**
   - The `refreshToken` secret is required, like every required secret ([03](../spec/03-configuration.md)): an account cannot be deployed before its first `tapyr auth login`, and a missing secret is a configuration error, also when a stored pair exists. The order is login first, then deploy; `tapyr validate --check-secrets` says which secret is missing and names the command.
   - The stored pair is keyed by provider and account only, not by a fingerprint of the auth block as other tokens are: a package upgrade that changes the scopes, the token parameters or `refreshBefore` keeps the pair instead of falling back to a spent bootstrap token. A token that lacks a newly needed scope is rejected by the API (403) and needs a new login, which the release notes of the package say.
   - It starts from the stored token pair when the hash of the bootstrap `refreshToken` secret matches the one the pair descends from, and from the secret otherwise. A changed secret (a new `tapyr auth login`) therefore replaces the stored pair at the next reload or restart; file secrets are re-read on reload ([03](../spec/03-configuration.md)).
   - It refreshes `refreshBefore` the expiry, one refresh at a time per account, through the budget engine with `authRequest: true`.
   - When a refresh answers a refresh token, the new pair is written to the state store before the access token is used; when it answers none, the previous refresh token is kept. Rotation is detected, not declared.
   - Only a refresh rejected with `invalid_grant` ([RFC 6749, section 5.2](https://www.rfc-editor.org/rfc/rfc6749#section-5.2)) sets the re-bootstrap condition: `tapyr_auth_reauth_required`, a `/readyz` reason and a status API message naming the account and `tapyr auth login`. The runs of the account then fail with the re-bootstrap error without calling the token endpoint again, until the bootstrap secret changes. Other rejections of the token endpoint (`invalid_client` for a wrong or rotated client secret, any other 400, 401 or 403) leave the stored pair and the condition alone: they are credential errors with the rejection backoff of [05](../spec/05-collection.md#error-classification), and fixing the client secret is enough. This is narrower than the Aruba plugin, which latches on any 400 or 401.
   - Stored pairs are sealed with `TAPYR_STATE_KEY` like every token (warning once when it is not set).
   - Read-only (`tapyr record` next to `tapyr run`): it never refreshes, since the refresh token may rotate. It uses the stored access token until its expiry, then fails with a message.

3. **`tapyr auth login <account>` produces the bootstrap secret; it never touches the state store.**

   ```sh
   tapyr auth login strava-home --config config.yaml --secret-file ./strava-refresh-token
   ```

   - It loads the configuration, compiles the account without its `refreshToken` secret (the one being produced) and resolves its client id and secret.
   - It builds the authorize URL with a random `state`, the PKCE challenge, the scopes and the redirect URL, and prints it. The URL holds no secret.
   - **Paste, the default**: the person opens the URL in any browser, on any machine, consents, and pastes the URL the browser was redirected to (or only its `code`). Nothing needs to listen at the redirect URL, so it works over SSH and with any redirect URL the vendor accepts. Tapyr checks `state`, then exchanges the code.
   - **`--listen 127.0.0.1:8085`**: Tapyr serves the redirect itself, for a redirect URL on the loopback interface of the machine where the browser runs.
   - **`--device`**: the device authorization grant ([RFC 8628](https://www.rfc-editor.org/rfc/rfc8628)), for a package that declares `deviceAuthorizationURL`: Tapyr prints a code and a URL, and polls the token endpoint at the interval the server asks for.
   - The code exchange and the device polling go through an in-memory budget engine built from the provider limits: the budget engine is never bypassed, and the login command opens no state store, so it can run anywhere, while `tapyr run` runs.
   - It writes the refresh token to `--secret-file`, created with mode 0600, and refuses an existing file without `--force`. It never prints a token. The access token of the exchange is discarded: `tapyr run` obtains its own with one refresh.
   - It ends with what to do next: give the file to the secret the package names (`{ file: ... }`, an environment variable, a Kubernetes Secret), then reload or restart `tapyr run`.

4. **The simulator serves the flow**, declared in `auth.accept` of `sim/sim.yaml`: an authorize endpoint that consents at once (it redirects to `redirect_uri` with a code and the `state`), a token endpoint for the `authorization_code`, `refresh_token` and device code grants that checks the client, the PKCE verifier and the redirect URL, rotates refresh tokens when asked to, rejects a spent refresh token with `invalid_grant`, and accepts a fixed bootstrap refresh token (`sim-...`) for contract tests. `tapyr auth login` against the simulator, then `tapyr run`, restarts and rotations are tested end to end with the fake clock.

5. **The Aruba plugin stays as it is for now.** Its flow is an instance of this one (a bootstrap refresh token from the Central UI, rotation, client credentials), except for the details of its token request. Moving `aruba-central-classic` to the declarative type is a separate change, made only if the declarative type expresses its token request without special cases, and verified against the real platform again.

## Alternatives considered

- **`tapyr auth login` writes the token pair into the state store.** Transparent once it works, but the login must then run where the state store is (a pod, a headless server, a read-only container), with its `TAPYR_STATE_KEY`, while `tapyr run` holds the database. The secret file moves the handoff to the place where operators already put secrets, and reuses the bootstrap rules of the Aruba plugin, verified in production.
- **A write endpoint on the status API to hand the token to `tapyr run`.** The status API is read-only by design, and an endpoint that accepts credentials would need its own authentication.
- **Loopback redirect only.** The usual flow of desktop tools, but the browser must run on the machine that listens, and some vendors only accept the redirect URL registered for the application. It stays an option (`--listen`).
- **Device code only.** No redirect at all, but few of the target APIs offer it (Strava and Netatmo do not).
- **A Go plugin per API.** What Aruba needed in 2026-11; repeating it for every API contradicts "declarative first" and keeps such APIs out of community packages.
- **A `rotates: true` field.** A refresh that answers a refresh token is a rotation; detecting it removes a field that a package author could get wrong, with the losing case being a lost refresh token.
- **Starting an account without its bootstrap secret, degraded until the login.** It would let a configuration be deployed first, but it needs a second kind of missing secret in compilation and validation, and it hides a mistake that the configuration error makes obvious.
- **Keying the stored pair by a fingerprint of the auth block**, as other tokens are. A package upgrade would then orphan every rotated pair and require a new login for every account of the package.
- **One token per target.** Targets share the account's credentials everywhere else; one account per person keeps the store, the status and the login command per account.
- **Writing the access token to the secret file too.** It expires within hours, so it would be stale by the time the file is deployed, and it would be a second secret to protect.

## Consequences

- Packages for authorization-code APIs become YAML only, including community packages; the provider ideas beyond networks (energy, sport, the connected home) open up.
- Operators run one interactive command per account, once, then whenever a refresh token is revoked or expires unused (some vendors expire it after weeks without use). The re-bootstrap condition and its message tell them when.
- The secret file holds a long-lived credential: the docs say to treat it as the client secret is treated, and to delete it once given to the secret store.
- The refresh token in the secret goes stale after the first rotation; that is expected (the stored pair wins while the secret is unchanged), and the docs say so, as for Aruba. Losing the state store with a rotated pair means a new `tapyr auth login`.
- Two Tapyr instances on the same account and a rotating refresh token would spend each other's tokens; this stays a single-instance design, as for every quota.
- The simulator gains an authorize endpoint and device code polling; the auth part of [08](../spec/08-simulator.md) and the auth types of [05](../spec/05-collection.md) and [01](../spec/01-vision-and-scope.md) are updated with the implementation.
