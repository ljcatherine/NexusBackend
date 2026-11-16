# Nexus sign-in (Keycloak)

A self-hosted sign-in page for Nexus that looks like the rest of the site. Cloudflare Access
uses this Keycloak realm as its only identity provider with **instant authentication**, so
signing in to any Nexus app goes straight to this page and never shows Cloudflare's own
login screen. Access still issues the usual signed assertion afterwards, so the Worker's
owner check, the gateway's verification and the apps work unchanged. This works on the free
Cloudflare Zero Trust plan.

**Signing in takes your password, then a 6-digit code from the NexusiOS app** (Services →
Nexus sign-in). There are no passkeys, no SMS and no self-service account page.

```
Sign in button ──► Cloudflare Access (instant auth, requires MFA)
                ──► https://auth.example.com/realms/nexus/…   ← password, then NexusiOS code
                ──► back to Access ──► Worker ──► gateway
```

| File | What it is |
|---|---|
| [`login-policy.sh`](login-policy.sh) | Applies the sign-in policy with `kcadm.sh`: password + code required, passkeys removed, account console off, AMR claim, CSP, events. Backs up first; has `check` and `rollback`. |
| [`themes/nexus/login/`](themes/nexus/login) | The Nexus login theme. Extends Keycloak's own theme, so every page (password, code, code setup, errors) keeps working. |
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
5. Import `realm-nexus.example.json` with your Access team domain in the redirect URI, and
   create your one user (your owner email, verified) with a long password.
6. Apply the sign-in policy (next section).

## Sign-in policy: password + NexusiOS code

Install the NexusiOS build with **Services → Nexus sign-in** first. Then, on the server:

```sh
sudo -u keycloak ./login-policy.sh backup
sudo -u keycloak ./login-policy.sh otp
```

`otp` makes the code required after the password, turns passkeys off on the login page, turns
off the account console, adds the AMR claim and sets the CSP. Then sign in once: Keycloak shows
a QR code, which you scan in NexusiOS → Services → Nexus sign-in, and you type the code. When
signing in with the code works, remove stored passkeys:

```sh
sudo -u keycloak ./login-policy.sh remove-passkeys
sudo -u keycloak ./login-policy.sh check
```

`check` should report `LOGIN PAGE: no passkey or WebAuthn code`. If you get locked out, run
`./login-policy.sh rollback` on the server (it puts Keycloak's built-in flow back).

**Lost phone:** sign in to the admin console over SSH (`ssh -L 8180:127.0.0.1:8180 <server>`,
then `http://127.0.0.1:8180/admin/`), open realm `nexus` → Users → you → Credentials, delete the
OTP credential, and sign in again to set up the new phone.

## Publish only the sign-in pages

In Cloudflare: **Networks → Tunnels → your tunnel → Public hostnames → Add**:

| Field | Value |
|---|---|
| Subdomain / domain | `auth` / your domain |
| Path | `^/(realms/nexus/(protocol/openid-connect/(auth\|token\|certs\|logout\|userinfo)\|login-actions/[a-z-]+\|\.well-known/openid-configuration)\|resources/)` |
| Service | `http://127.0.0.1:8180` |

Only what Access needs to sign you in is published. The admin console (`/admin`), the `master`
realm, the account console, client registration, device and CIBA endpoints stay unreachable.

## Connect Cloudflare Access

**Integrations → Identity providers → Add new → OpenID Connect**:

| Field | Value |
|---|---|
| App ID / Client secret | `cloudflare-access` and its secret from Keycloak's **Credentials** tab |
| Auth URL | `https://auth.example.com/realms/nexus/protocol/openid-connect/auth` |
| Token URL | `https://auth.example.com/realms/nexus/protocol/openid-connect/token` |
| Certificate URL | `https://auth.example.com/realms/nexus/protocol/openid-connect/certs` |
| PKCE | On |
| OIDC claims | `email`, `amr` |

Then, on each Access application: select only this provider under **Login methods**, turn on
**Instant authentication**, and keep the Allow policy's owner email. Add a **Require** rule:
**Authentication method → `mfa`**, so Access refuses any login that skipped the code. Set the
session duration to 8 hours or less (2 hours for mail and SSH).

> [!WARNING]
> This Keycloak realm is part of your security boundary: anyone who can sign in here gets past
> Access. Keep it updated, keep the code required and self-registration off. The Cloudflare
> dashboard is the break-glass path if Keycloak is ever down, so protect the Cloudflare account
> with a hardware key or an authenticator app.
