# Nexus sign-in (Keycloak)

A self-hosted sign-in page for Nexus that looks like the rest of the site. Cloudflare Access
uses this Keycloak realm as its only identity provider with **instant authentication**, so
signing in to any Nexus app goes straight to this page and never shows Cloudflare's own
login screen. Access still issues the usual signed assertion afterwards, so the Worker's
owner check, the gateway's verification and the apps work unchanged. This works on the free
Cloudflare Zero Trust plan.

```
Sign in button ──► Cloudflare Access (instant auth)
                ──► https://auth.example.com/realms/nexus/…   ← this theme
                ──► back to Access ──► Worker ──► gateway
```

| File | What it is |
|---|---|
| [`themes/nexus/login/`](themes/nexus/login) | The Nexus login theme. Extends Keycloak's own theme, so every page (password, passkeys, OTP, errors) keeps working. |
| [`realm-nexus.example.json`](realm-nexus.example.json) | Realm import: Nexus theme, no self-registration, brute-force protection, the `cloudflare-access` OIDC client. |
| [`keycloak.service`](keycloak.service) | Hardened systemd unit. |
| [`keycloak.env.example`](keycloak.env.example) | Environment template for `/etc/keycloak/keycloak.env`. |

Tested with Keycloak **26.8.1**.

## Install on the server

1. Install Keycloak 26.8.1 to `/opt/keycloak` (verify the release checksum), owned by a
   system user `keycloak`, with Java 21 and a local PostgreSQL database `keycloak`.
2. Copy the theme: `cp -r themes/nexus /opt/keycloak/themes/`.
3. Copy `keycloak.env.example` to `/etc/keycloak/keycloak.env` (root:keycloak, 0640) and fill
   it in, then build once: `sudo -u keycloak /opt/keycloak/bin/kc.sh build --db=postgres --health-enabled=true`.
4. Install `keycloak.service`, then start it. Create the bootstrap admin on the console only,
   reached through `KC_HOSTNAME_ADMIN` on loopback.
5. Import `realm-nexus.example.json` with your Access team domain in the redirect URI, create
   your one user (your owner email, verified), and require a passkey for it.

## Publish only the sign-in pages

In Cloudflare: **Networks → Tunnels → your tunnel → Public hostnames → Add**:

| Field | Value |
|---|---|
| Subdomain / domain | `auth` / your domain |
| Path | `^/(realms/nexus/\|resources/)` |
| Service | `http://127.0.0.1:8180` |

The admin console (`/admin`) and the `master` realm are not published.

## Connect Cloudflare Access

**Integrations → Identity providers → Add new → OpenID Connect**:

| Field | Value |
|---|---|
| App ID / Client secret | `cloudflare-access` and its secret from Keycloak's **Credentials** tab |
| Auth URL | `https://auth.example.com/realms/nexus/protocol/openid-connect/auth` |
| Token URL | `https://auth.example.com/realms/nexus/protocol/openid-connect/token` |
| Certificate URL | `https://auth.example.com/realms/nexus/protocol/openid-connect/certs` |
| PKCE | On |
| OIDC claims | `email` |

Then, on each Access application: select only this provider under **Login methods** and turn
on **Instant authentication**. The Allow policy must still include your owner email.

> [!WARNING]
> This Keycloak realm is now part of your security boundary: anyone who can sign in here gets
> past Access. Keep it updated, require a passkey, and keep self-registration off. If it is
> ever down, re-enable another login method in the Cloudflare dashboard (which does not sign
> in through Access).
