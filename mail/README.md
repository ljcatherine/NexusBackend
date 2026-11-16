# Nexus Mail service

The mailbox behind [mail.liamjc.dev](https://mail.liamjc.dev). Mail and attachments are stored
only on this Debian box. Python standard library only, no packages to install.

```
incoming:  sender → Cloudflare Email Routing → nexus-online Worker → tunnel → 127.0.0.1:8898 /inbound
reading:   browser → Cloudflare Access → nexus-online Worker → tunnel → 127.0.0.1:8898
sending:   127.0.0.1:8898 /send → Brevo SMTP relay (port 587, STARTTLS) → recipient
```

- **Owner only.** The Worker checks your Cloudflare Access sign-in on every request. The
  service accepts only loopback connections that carry `NEXUS_MAIL_TOKEN`.
- **Nothing listens publicly.** Cloudflare reaches the service through the existing tunnel.
- **If Debian is offline**, the Worker holds incoming mail in D1 and hands it over within
  5 minutes of the service coming back. Messages over about 1.9 MB are refused instead, so the
  sender's server retries later.
- **Sending** goes through [Brevo](https://www.brevo.com)'s free plan (300 emails a day).
  Brevo sees what you send. Turn off open and click tracking in Brevo so it adds none.

## What it does

Inbox, Starred, Sent, Drafts, All mail and Trash. Conversations are threaded by `References`
and `In-Reply-To`, falling back to subject and correspondent. Send, reply, reply all and
forward, including forwarding the original attachments. Cc, Bcc, attachments up to 25 MB per
message, search, stars, archive, trash with restore, and delete forever.

Not yet: rich-text formatting (messages are sent as plain text), labels, a spam filter, and
attachments in saved drafts.

## 1. Brevo

1. Create a free account at brevo.com.
2. **Senders, Domains & Dedicated IPs → Domains → Add a domain:** `liamjc.dev`. Brevo lists
   DNS records (a `brevo-code` TXT record, DKIM, and DMARC). Add each one in Cloudflare →
   liamjc.dev → **DNS**, then click **Authenticate** in Brevo.
3. Add `include:spf.brevo.com` to the domain's SPF record, keeping Cloudflare's:
   `v=spf1 include:_spf.mx.cloudflare.net include:spf.brevo.com ~all`
4. **Senders → Add a sender:** `liam@liamjc.dev`.
5. **SMTP & API → SMTP:** note the **login** (looks like `1234ab001@smtp-brevo.com`) and
   **Generate a new SMTP key**. You'll paste both into the env file in step 2.
6. In Brevo's transactional email settings, turn off **open tracking** and **click tracking**.

## 2. Debian

From a checkout of this repository on the Debian box:

```sh
sudo useradd --system --home /nonexistent --shell /usr/sbin/nologin nexus-mail
sudo install -d -m 755 /opt/nexus-mail
sudo install -m 644 mail/server.py /opt/nexus-mail/server.py
sudo install -d -m 750 -g nexus-mail /etc/nexus-mail
sudo install -m 640 -g nexus-mail mail/.env.example /etc/nexus-mail/nexus-mail.env
openssl rand -hex 32
```

Copy the line `openssl` printed, then fill in the env file:

```sh
sudoedit /etc/nexus-mail/nexus-mail.env
```

| Setting | Value |
|---|---|
| `NEXUS_MAIL_TOKEN` | The `openssl` output. The Worker gets the same value in step 4 |
| `NEXUS_MAIL_FROM` | `liam@liamjc.dev` (comma-separate more addresses) |
| `NEXUS_MAIL_SMTP_USER` / `NEXUS_MAIL_SMTP_PASSWORD` | Brevo SMTP login and key |

Start it:

```sh
sudo install -m 644 mail/nexus-mail.service /etc/systemd/system/nexus-mail.service
sudo systemctl daemon-reload
sudo systemctl enable --now nexus-mail
systemctl status nexus-mail --no-pager
```

Mail lives in `/var/lib/nexus-mail` (`mail.db` plus an `attachments/` folder). Back that folder
up; there is no other copy.

## 3. Cloudflare tunnel route

The Worker reaches the service through a Workers VPC service on the same tunnel the gateway
uses. On your PC, in the NexusOnline folder (`npx wrangler vpc service list` shows the tunnel ID):

```sh
npx wrangler vpc service create nexus-mail --type http --tunnel-id <tunnel id> --ipv4 127.0.0.1 --http-port 8898
```

Put the new service's ID in `wrangler.jsonc` under `vpc_services` as `NEXUS_MAIL`.

## 4. Worker secret and deploy

In the NexusOnline folder:

```sh
npx wrangler secret put MAIL_SERVICE_TOKEN
```

Paste the same token as `NEXUS_MAIL_TOKEN`. Then deploy with `npm run deploy` and open
mail.liamjc.dev.

## Tests

```sh
cd mail && python3 -m unittest test_server
```
