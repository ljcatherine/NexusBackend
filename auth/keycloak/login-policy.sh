#!/usr/bin/env bash
# Nexus sign-in policy for the Keycloak realm `nexus`: password + 6-digit code from the NexusiOS app,
# passkeys removed. Run on the Keycloak host as an admin who can reach the loopback admin console.
#
#   sudo -u keycloak ./login-policy.sh backup          # save realm settings and flows (always first)
#   sudo -u keycloak ./login-policy.sh otp             # phase 1: password + code required, passkeys off at login
#   ... sign in once, scan the QR code with NexusiOS → Services → Nexus sign-in, enter the code ...
#   sudo -u keycloak ./login-policy.sh remove-passkeys # phase 2: delete stored passkeys, disable enrollment
#   sudo -u keycloak ./login-policy.sh check           # show the live state
#   sudo -u keycloak ./login-policy.sh rollback        # put the built-in browser flow back (emergency)
#
# Needs: /opt/keycloak/bin/kcadm.sh, python3, curl. Asks for the master-realm admin password once.
set -euo pipefail

REALM=nexus
SERVER=${KC_SERVER:-http://127.0.0.1:8180}
KCADM=${KCADM:-/opt/keycloak/bin/kcadm.sh}
FLOW=nexus-browser
FORMS=nexus-browser-forms
BACKUPS=${KC_BACKUPS:-/opt/keycloak/data/login-policy-backups}
CFG=$(mktemp -d)/kcadm.config
trap 'rm -rf "$(dirname "$CFG")"' EXIT

# Keycloak's login pages use inline module scripts, so scripts need 'unsafe-inline'; everything else is locked down.
CSP="default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; img-src 'self' data:; connect-src 'self'; frame-src 'self'; frame-ancestors 'self'; object-src 'none'; base-uri 'none'; form-action 'self' https://*.cloudflareaccess.com"

kc() { "$KCADM" "$@" --config "$CFG"; }
json() { python3 -c "import json,sys; d=json.load(sys.stdin); $1"; }
say() { printf '\n== %s\n' "$*"; }

login() {
  say "Signing in to the Keycloak admin API at $SERVER (master realm)"
  read -rp "Admin username: " admin
  # No output redirection here: kcadm (Java) only prompts for the password when stdout is a
  # terminal, otherwise it fails with "Console is not active, but password is required".
  kc config credentials --server "$SERVER" --realm master --user "$admin"
}

backup() {
  local dir; dir="$BACKUPS/$(date +%Y%m%d-%H%M%S)"
  mkdir -p "$dir" && chmod 700 "$BACKUPS" "$dir"
  kc get "realms/$REALM" > "$dir/realm.json"
  kc get authentication/flows -r "$REALM" > "$dir/flows.json"
  kc get authentication/required-actions -r "$REALM" > "$dir/required-actions.json"
  kc get clients -r "$REALM" --fields id,clientId,enabled > "$dir/clients.json"
  say "Backup saved in $dir (realm settings, flows, required actions, clients; no user secrets)"
}

exec_id() { # exec_id <flow alias> <providerId or displayName>
  kc get "authentication/flows/$1/executions" -r "$REALM" |
    json "print(next(e['id'] for e in d if e.get('providerId')=='$2' or e.get('displayName')=='$2'))"
}

require() { # require <flow alias> <execution id> <REQUIRED|ALTERNATIVE|DISABLED>
  kc update "authentication/flows/$1/executions" -r "$REALM" -b "{\"id\":\"$2\",\"requirement\":\"$3\"}"
}

client_uuid() { kc get clients -r "$REALM" -q clientId="$1" --fields id | json "print(d[0]['id'] if d else '')"; }

otp() {
  backup
  say "Realm policy: 6-digit TOTP every 30 s (SHA-1, works with NexusiOS), no code reuse, lockout, events, CSP"
  kc update "realms/$REALM" \
    -s otpPolicyType=totp -s otpPolicyAlgorithm=HmacSHA1 -s otpPolicyDigits=6 -s otpPolicyPeriod=30 \
    -s otpPolicyLookAheadWindow=1 -s otpPolicyCodeReusable=false \
    -s registrationAllowed=false -s resetPasswordAllowed=false -s rememberMe=false \
    -s bruteForceProtected=true -s failureFactor=5 -s permanentLockout=false \
    -s ssoSessionIdleTimeout=1800 -s ssoSessionMaxLifespan=36000 \
    -s eventsEnabled=true -s eventsExpiration=2592000 -s adminEventsEnabled=true -s adminEventsDetailsEnabled=true \
    -s "browserSecurityHeaders.contentSecurityPolicy=$CSP" \
    -s webAuthnPolicyPasswordlessPasskeysEnabled=false

  if kc get authentication/flows -r "$REALM" | json "sys.exit(0 if any(f['alias']=='$FLOW' for f in d) else 1)"; then
    say "Flow $FLOW already exists, keeping it"
  else
    say "Creating flow $FLOW: cookie, or (password then code), both required, no passkeys"
    kc create authentication/flows -r "$REALM" -s alias="$FLOW" -s providerId=basic-flow -s topLevel=true -s builtIn=false \
      -s description="Nexus: password then 6-digit code from NexusiOS. No passkeys."
    kc create "authentication/flows/$FLOW/executions/execution" -r "$REALM" -s provider=auth-cookie >/dev/null
    require "$FLOW" "$(exec_id "$FLOW" auth-cookie)" ALTERNATIVE
    kc create "authentication/flows/$FLOW/executions/flow" -r "$REALM" -s alias="$FORMS" -s type=basic-flow \
      -s provider=registration-page-form -s description="Password then code" >/dev/null
    require "$FLOW" "$(exec_id "$FLOW" "$FORMS")" ALTERNATIVE
    kc create "authentication/flows/$FORMS/executions/execution" -r "$REALM" -s provider=auth-username-password-form >/dev/null
    require "$FORMS" "$(exec_id "$FORMS" auth-username-password-form)" REQUIRED
    kc create "authentication/flows/$FORMS/executions/execution" -r "$REALM" -s provider=auth-otp-form >/dev/null
    require "$FORMS" "$(exec_id "$FORMS" auth-otp-form)" REQUIRED
  fi

  say "Binding $FLOW as the browser flow"
  kc update "realms/$REALM" -s browserFlow="$FLOW"

  say "Adding the AMR claim (pwd, otp) to tokens for Cloudflare Access"
  local cid; cid=$(client_uuid cloudflare-access)
  if [ -z "$cid" ]; then echo "Client cloudflare-access not found; skipping AMR mapper"; else
    if ! kc get "clients/$cid/protocol-mappers/models" -r "$REALM" | json "sys.exit(0 if any(m['protocolMapper']=='oidc-amr-mapper' for m in d) else 1)"; then
      kc create "clients/$cid/protocol-mappers/models" -r "$REALM" -s name=amr -s protocol=openid-connect -s protocolMapper=oidc-amr-mapper \
        -s 'config."id.token.claim"=true' -s 'config."access.token.claim"=true' -s 'config."introspection.token.claim"=true' >/dev/null
    fi
  fi

  say "Turning off the self-service account console (credentials are managed in the admin console)"
  local c id
  for c in account-console account; do
    id=$(client_uuid "$c"); if [ -n "$id" ]; then kc update "clients/$id" -r "$REALM" -s enabled=false; fi
  done
  check
  cat <<'NEXT'

Phase 1 done. Next:
  1. Open https://nexus.liamjc.dev/app/ in a private window and sign in with your password.
  2. Keycloak shows "Set up your authenticator": in NexusiOS open Services → Nexus sign-in,
     tap Scan QR code (or paste the key from "Unable to scan?"), then type the 6-digit code.
  3. Sign out and in again to confirm the code is asked for. Then run:  ./login-policy.sh remove-passkeys
If anything goes wrong:  ./login-policy.sh rollback
NEXT
}

remove_passkeys() {
  backup
  say "Disabling passkey enrollment"
  local action
  for action in webauthn-register webauthn-register-passwordless; do
    kc update "authentication/required-actions/$action" -r "$REALM" -s enabled=false 2>/dev/null || echo "  ($action not present)"
  done
  kc update "realms/$REALM" -s webAuthnPolicyPasswordlessPasskeysEnabled=false

  say "Deleting stored passkeys and security keys"
  kc get users -r "$REALM" --fields id,username --limit 500 | json "[print(u['id'], u['username']) for u in d]" |
  while read -r uid name; do
    kc get "users/$uid/credentials" -r "$REALM" |
      json "[print(c['id'], c['type']) for c in d if c['type'] in ('webauthn','webauthn-passwordless')]" |
      while read -r credid type; do
        kc delete "users/$uid/credentials/$credid" -r "$REALM" && echo "  removed $type for $name"
      done
    kc get "users/$uid/credentials" -r "$REALM" | json "sys.exit(0 if any(c['type']=='otp' for c in d) else 1)" ||
      echo "  WARNING: $name has no code set up yet and will be asked to set one up at next sign-in"
  done
  check
  echo
  echo "Also remove saved passkeys for auth.liamjc.dev from iCloud Keychain, Google Password Manager and Windows Hello."
}

rollback() {
  say "Binding the built-in browser flow again (passkeys and password as before)"
  kc update "realms/$REALM" -s browserFlow=browser
  local c id
  for c in account-console account; do
    id=$(client_uuid "$c"); if [ -n "$id" ]; then kc update "clients/$id" -r "$REALM" -s enabled=true; fi
  done
  echo "Done. $FLOW is kept, so './login-policy.sh otp' re-applies it. Backups: $BACKUPS"
}

check() {
  say "Live state of realm $REALM"
  kc get "realms/$REALM" --fields browserFlow,otpPolicyType,otpPolicyAlgorithm,otpPolicyDigits,otpPolicyPeriod,otpPolicyCodeReusable,webAuthnPolicyPasswordlessPasskeysEnabled,registrationAllowed,resetPasswordAllowed,bruteForceProtected,eventsEnabled |
    json "[print(f'  {k} = {v}') for k,v in d.items()]"
  local flow; flow=$(kc get "realms/$REALM" --fields browserFlow | json "print(d['browserFlow'])")
  echo "  executions in $flow:"
  kc get "authentication/flows/$flow/executions" -r "$REALM" |
    json "[print('   ', '  '*e.get('level',0), e.get('displayName'), '=>', e.get('requirement')) for e in d]"
  kc get authentication/required-actions -r "$REALM" |
    json "[print(f\"  required action {a['alias']}: {'ENABLED' if a['enabled'] else 'disabled'}\") for a in d if 'webauthn' in a['alias'] or a['alias']=='CONFIGURE_TOTP']"
  local redirect; redirect=$(kc get clients -r "$REALM" -q clientId=cloudflare-access --fields redirectUris | json "print(d[0]['redirectUris'][0] if d else '')")
  if [ -n "$redirect" ]; then
    local encoded page
    encoded=$(python3 -c 'import sys,urllib.parse;print(urllib.parse.quote(sys.argv[1],safe=""))' "$redirect")
    page=$(curl -s --max-time 10 "$SERVER/realms/$REALM/protocol/openid-connect/auth?client_id=cloudflare-access&response_type=code&scope=openid&redirect_uri=$encoded&code_challenge=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA&code_challenge_method=S256&state=check")
    if grep -qiE 'webauthn|passkey' <<<"$page"; then echo "  LOGIN PAGE: still mentions passkeys/WebAuthn"; else echo "  LOGIN PAGE: no passkey or WebAuthn code"; fi
  fi
}

cmd=${1:-check}
case "$cmd" in
  backup|otp|remove-passkeys|rollback|check) ;;
  *) echo "usage: $0 backup|otp|remove-passkeys|check|rollback" >&2; exit 2 ;;
esac
login
case "$cmd" in
  backup) backup ;;
  otp) otp ;;
  remove-passkeys) remove_passkeys ;;
  rollback) rollback ;;
  check) check ;;
esac
