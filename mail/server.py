#!/usr/bin/env python3
"""Nexus Mail service: the owner's mailbox, stored only on this machine.

Incoming mail: Cloudflare Email Routing -> nexus-online Worker -> private tunnel -> POST /inbound.
Outgoing mail: POST /send -> SMTP relay (Brevo, port 587, STARTTLS).
The Worker checks the owner's Cloudflare Access sign-in before calling any route; this
service only accepts loopback connections that carry NEXUS_MAIL_TOKEN. Python stdlib only.
"""
from __future__ import annotations

import base64
import hmac
import json
import os
import re
import shutil
import smtplib
import sqlite3
import ssl
import sys
import time
import uuid
from contextlib import contextmanager
from email import policy
from email.message import EmailMessage
from email.parser import BytesParser
from email.utils import formataddr, getaddresses, make_msgid, parseaddr
from html import unescape
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from ipaddress import ip_address
from urllib.parse import parse_qs, quote, urlsplit

MB = 1024 * 1024
MAX_INBOUND = 30 * MB
MAX_SEND = 32 * MB  # JSON with base64 attachments; the relay caps the message itself
MAX_JSON = 1 * MB
MAX_TEXT = 1_000_000
MAX_RECIPIENTS = 50
PAGE = 50
FOLDERS = {
    'inbox': "folder = 'inbox'",
    'starred': "starred = 1 AND folder NOT IN ('trash', 'drafts')",
    'sent': "folder = 'sent'",
    'drafts': "folder = 'drafts'",
    'all': "folder IN ('inbox', 'archive', 'sent')",
    'trash': "folder = 'trash'",
}
ID = re.compile(r'^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$')
SUBJECT_PREFIX = re.compile(r'^\s*((re|fw|fwd|aw|sv)\s*(\[\d+\])?\s*:\s*)+', re.I)

SCHEMA = """
CREATE TABLE IF NOT EXISTS messages (
  id TEXT PRIMARY KEY,
  thread_id TEXT NOT NULL,
  folder TEXT NOT NULL,
  trashed_from TEXT NOT NULL DEFAULT '',
  starred INTEGER NOT NULL DEFAULT 0,
  read INTEGER NOT NULL DEFAULT 0,
  received_at INTEGER NOT NULL,
  message_id TEXT NOT NULL DEFAULT '',
  in_reply_to TEXT NOT NULL DEFAULT '',
  refs TEXT NOT NULL DEFAULT '',
  subject TEXT NOT NULL DEFAULT '',
  subject_key TEXT NOT NULL DEFAULT '',
  from_addr TEXT NOT NULL DEFAULT '',
  from_name TEXT NOT NULL DEFAULT '',
  to_json TEXT NOT NULL DEFAULT '[]',
  cc_json TEXT NOT NULL DEFAULT '[]',
  bcc_json TEXT NOT NULL DEFAULT '[]',
  reply_to TEXT NOT NULL DEFAULT '',
  envelope_to TEXT NOT NULL DEFAULT '',
  text TEXT NOT NULL DEFAULT '',
  html TEXT NOT NULL DEFAULT '',
  attachments TEXT NOT NULL DEFAULT '[]',
  reply_of TEXT NOT NULL DEFAULT '',
  forward_of TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS messages_thread ON messages (thread_id);
CREATE INDEX IF NOT EXISTS messages_message_id ON messages (message_id);
CREATE INDEX IF NOT EXISTS messages_folder ON messages (folder, received_at DESC);
"""


class Fail(Exception):
    def __init__(self, status: int, message: str):
        super().__init__(message)
        self.status = status


def now_ms() -> int:
    return int(time.time() * 1000)


def subject_key(subject: str) -> str:
    return SUBJECT_PREFIX.sub('', subject or '').strip().lower()


def plain(html: str) -> str:
    html = re.sub(r'(?is)<(style|script|head)\b.*?</\1>', ' ', html or '')
    return re.sub(r'\s+', ' ', unescape(re.sub(r'<[^>]*>', ' ', html))).strip()


def addresses(raw) -> list[dict]:
    out = []
    for name, addr in getaddresses([str(raw)] if isinstance(raw, str) else [str(r) for r in raw]):
        addr = addr.strip()
        if addr:
            out.append({'name': name.strip(), 'address': addr})
    return out


def clean_recipients(values) -> list[dict]:
    if not isinstance(values, list) or not all(isinstance(v, str) for v in values):
        raise Fail(400, 'Recipients must be a list of addresses.')
    out = addresses(values)
    for a in out:
        if not re.fullmatch(r'[^@\s<>,;"]+@[^@\s<>,;"]+\.[^@\s<>,;"]+', a['address']) or re.search(r'[\r\n]', a['name']):
            raise Fail(400, f"“{a['address']}” is not a valid email address.")
    return out


class Store:
    def __init__(self, directory: str):
        self.dir = directory
        os.makedirs(os.path.join(directory, 'attachments'), mode=0o700, exist_ok=True)
        with self.db() as db:
            db.executescript(SCHEMA)

    @contextmanager
    def db(self):
        """One connection per use: commits on success, rolls back on error, always closes."""
        conn = sqlite3.connect(os.path.join(self.dir, 'mail.db'), timeout=30)
        try:
            conn.row_factory = sqlite3.Row
            conn.execute('PRAGMA journal_mode=WAL')
            with conn:
                yield conn
        finally:
            conn.close()

    # --- attachments live on disk: attachments/<message id>/<index> ---
    def save_attachments(self, message_id: str, items: list[tuple[str, str, bytes]]) -> list[dict]:
        meta = []
        if not items:
            return meta
        folder = os.path.join(self.dir, 'attachments', message_id)
        os.makedirs(folder, mode=0o700, exist_ok=True)
        for i, (filename, mime, data) in enumerate(items):
            with open(os.path.join(folder, str(i)), 'wb') as f:
                f.write(data)
            meta.append({'filename': filename[:200] or f'attachment-{i + 1}', 'type': mime[:100], 'size': len(data)})
        return meta

    def attachment(self, message_id: str, index: int) -> tuple[dict, bytes]:
        with self.db() as db:
            row = db.execute('SELECT attachments FROM messages WHERE id = ?', (message_id,)).fetchone()
        meta = json.loads(row['attachments']) if row else []
        if not 0 <= index < len(meta):
            raise Fail(404, 'Attachment not found.')
        with open(os.path.join(self.dir, 'attachments', message_id, str(index)), 'rb') as f:
            return meta[index], f.read()

    def drop_files(self, message_ids) -> None:
        for mid in message_ids:
            shutil.rmtree(os.path.join(self.dir, 'attachments', mid), ignore_errors=True)

    # --- threading: References / In-Reply-To first, then same subject with the same person ---
    def find_thread(self, db, refs: list[str], subject: str, person: str) -> str | None:
        refs = [r for r in refs if r][-100:]
        if refs:
            marks = ','.join('?' * len(refs))
            row = db.execute(f"SELECT thread_id FROM messages WHERE message_id IN ({marks}) AND folder != 'drafts' LIMIT 1", refs).fetchone()
            if row:
                return row['thread_id']
        key = subject_key(subject)
        if key and person:
            # shortcut: relays may rewrite our Message-ID, so a reply is matched by subject and correspondent.
            like = '%' + json.dumps(person.lower())[1:-1] + '%'
            row = db.execute("""SELECT thread_id FROM messages WHERE subject_key = ? AND folder != 'drafts' AND received_at > ?
                AND (lower(from_addr) = ? OR lower(to_json) LIKE ? OR lower(cc_json) LIKE ?) ORDER BY received_at DESC LIMIT 1""",
                (key, now_ms() - 60 * 86400_000, person.lower(), like, like)).fetchone()
            if row:
                return row['thread_id']
        return None

    def receive(self, raw: bytes, envelope_from: str, envelope_to: str) -> dict:
        msg = BytesParser(policy=policy.default).parsebytes(raw)
        message_id = str(msg.get('message-id', '') or '').strip()[:998]
        with self.db() as db:
            if message_id:
                dup = db.execute("SELECT id, thread_id FROM messages WHERE message_id = ? AND envelope_to = ? AND folder != 'sent'",
                                 (message_id, envelope_to)).fetchone()
                if dup:  # the Worker's queue may redeliver after a timeout
                    return dict(dup)
        sender = addresses(str(msg.get('from', '') or ''))
        sender = sender[0] if sender else {'name': '', 'address': envelope_from}
        text_part = msg.get_body(preferencelist=('plain',))
        html_part = msg.get_body(preferencelist=('html',))
        text = text_part.get_content() if text_part else ''
        html = html_part.get_content() if html_part else ''
        files = []
        for part in msg.iter_attachments():
            data = part.get_payload(decode=True) or b''
            files.append((str(part.get_filename() or ''), part.get_content_type(), data))
        in_reply_to = str(msg.get('in-reply-to', '') or '').strip()[:998]
        refs = str(msg.get('references', '') or '').split()[-100:]
        subject = str(msg.get('subject', '') or '')[:998]
        mid = str(uuid.uuid4())
        meta = self.save_attachments(mid, files)
        with self.db() as db:
            thread = self.find_thread(db, refs + [in_reply_to], subject, sender['address']) or mid
            db.execute("""INSERT INTO messages (id, thread_id, folder, received_at, message_id, in_reply_to, refs, subject, subject_key,
                from_addr, from_name, to_json, cc_json, reply_to, envelope_to, text, html, attachments)
                VALUES (?, ?, 'inbox', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
                (mid, thread, now_ms(), message_id, in_reply_to, ' '.join(refs), subject, subject_key(subject),
                 sender['address'][:320], sender['name'][:200], json.dumps(addresses(msg.get_all('to', []))),
                 json.dumps(addresses(msg.get_all('cc', []))), str(msg.get('reply-to', '') or '')[:998],
                 envelope_to[:320], text[:MAX_TEXT], html[:MAX_TEXT], json.dumps(meta)))
        return {'id': mid, 'thread_id': thread}

    def threads(self, folder: str, q: str, offset: int) -> dict:
        if folder not in FOLDERS:
            raise Fail(400, 'Unknown folder.')
        where, args = FOLDERS[folder], []
        if q:
            like = '%' + q.replace('\\', '\\\\').replace('%', '\\%').replace('_', '\\_') + '%'
            where += " AND (subject LIKE ? ESCAPE '\\' OR from_addr LIKE ? ESCAPE '\\' OR from_name LIKE ? ESCAPE '\\' OR to_json LIKE ? ESCAPE '\\' OR text LIKE ? ESCAPE '\\')"
            args += [like] * 5
        with self.db() as db:
            # shortcut: groups in Python over the newest 5000 matches; fine for one person's mailbox.
            rows = db.execute(f"""SELECT id, thread_id, folder, received_at, subject, from_addr, from_name, to_json,
                substr(CASE WHEN text != '' THEN text ELSE html END, 1, 600) AS body
                FROM messages WHERE {where} ORDER BY received_at DESC LIMIT 5000""", args).fetchall()
            latest, order = {}, []
            for r in rows:
                if r['thread_id'] not in latest:
                    latest[r['thread_id']] = r
                    order.append(r['thread_id'])
            page = order[offset:offset + PAGE]
            stats = {}
            if page:
                scope = "folder = 'trash'" if folder == 'trash' else "folder = 'drafts'" if folder == 'drafts' else "folder NOT IN ('trash', 'drafts')"
                marks = ','.join('?' * len(page))
                for s in db.execute(f"""SELECT thread_id, COUNT(*) n, SUM(read = 0) unread, MAX(starred) starred,
                    MAX(attachments != '[]') files,
                    group_concat(CASE WHEN folder IN ('sent', 'drafts') THEN '' ELSE coalesce(nullif(from_name, ''), from_addr) END, char(31)) people
                    FROM (SELECT * FROM messages WHERE thread_id IN ({marks}) AND {scope} ORDER BY received_at)
                    GROUP BY thread_id""", page):
                    stats[s['thread_id']] = s
            counts = db.execute("""SELECT (SELECT COUNT(DISTINCT thread_id) FROM messages WHERE folder = 'inbox' AND read = 0) inbox,
                (SELECT COUNT(*) FROM messages WHERE folder = 'drafts') drafts""").fetchone()
        out = []
        for tid in page:
            r, s = latest[tid], stats.get(tid)
            people, seen = [], set()
            for p in (s['people'] or '').split('\x1f') if s else []:
                p = p or 'me'
                if p not in seen:
                    seen.add(p)
                    people.append(p)
            body = r['body'] or ''
            out.append({
                'thread_id': tid, 'id': r['id'], 'folder': r['folder'], 'received_at': r['received_at'],
                'subject': r['subject'], 'from_addr': r['from_addr'], 'from_name': r['from_name'],
                'to': json.loads(r['to_json']), 'people': people[-4:] or ['me'],
                'snippet': (plain(body) if '<' in body else re.sub(r'\s+', ' ', body)).strip()[:200],
                'count': s['n'] if s else 1, 'unread': bool(s and s['unread']), 'starred': bool(s and s['starred']),
                'has_attachments': bool(s and s['files']),
            })
        return {'threads': out, 'more': len(order) > offset + PAGE, 'total': len(order),
                'counts': {'inbox': counts['inbox'], 'drafts': counts['drafts']}}

    def thread(self, thread_id: str) -> dict:
        with self.db() as db:
            rows = db.execute("SELECT * FROM messages WHERE thread_id = ? AND folder != 'drafts' ORDER BY received_at", (thread_id,)).fetchall()
        if not rows:
            raise Fail(404, 'Conversation not found.')
        return {'thread_id': thread_id, 'messages': [self.public(r) for r in rows]}

    @staticmethod
    def public(r) -> dict:
        m = dict(r)
        for k in ('to_json', 'cc_json', 'bcc_json'):
            m[k[:-5]] = json.loads(m.pop(k))
        m['attachments'] = json.loads(m['attachments'])
        m['starred'], m['read'] = bool(m['starred']), bool(m['read'])
        for k in ('subject_key', 'refs', 'trashed_from'):
            m.pop(k, None)
        return m

    def act(self, thread_id: str, action: str) -> dict:
        updates = {
            'read': "UPDATE messages SET read = 1 WHERE thread_id = ? AND folder != 'drafts'",
            'unread': "UPDATE messages SET read = 0 WHERE thread_id = ? AND folder != 'drafts'",
            'star': "UPDATE messages SET starred = 1 WHERE thread_id = ? AND folder != 'drafts'",
            'unstar': "UPDATE messages SET starred = 0 WHERE thread_id = ? AND folder != 'drafts'",
            'archive': "UPDATE messages SET folder = 'archive' WHERE thread_id = ? AND folder = 'inbox'",
            'inbox': "UPDATE messages SET folder = 'inbox' WHERE thread_id = ? AND folder = 'archive'",
            'trash': "UPDATE messages SET trashed_from = folder, folder = 'trash' WHERE thread_id = ? AND folder NOT IN ('trash', 'drafts')",
            'untrash': "UPDATE messages SET folder = trashed_from, trashed_from = '' WHERE thread_id = ? AND folder = 'trash'",
        }
        if action != 'delete' and action not in updates:
            raise Fail(404, 'Unknown action.')
        if not self.exists(thread_id):
            raise Fail(404, 'Conversation not found.')
        ids = []
        with self.db() as db:
            if action == 'delete':  # permanent, and only for what is already in the trash
                ids = [r['id'] for r in db.execute("SELECT id FROM messages WHERE thread_id = ? AND folder = 'trash'", (thread_id,))]
                db.execute("DELETE FROM messages WHERE thread_id = ? AND folder = 'trash'", (thread_id,))
                changed = len(ids)
            else:
                changed = db.execute(updates[action], (thread_id,)).rowcount
        self.drop_files(ids)
        return {'ok': True, 'changed': changed}

    def exists(self, thread_id: str) -> bool:
        with self.db() as db:
            return db.execute('SELECT 1 FROM messages WHERE thread_id = ? LIMIT 1', (thread_id,)).fetchone() is not None

    def get_draft(self, draft_id: str) -> dict:
        with self.db() as db:
            row = db.execute("SELECT * FROM messages WHERE id = ? AND folder = 'drafts'", (draft_id,)).fetchone()
        if not row:
            raise Fail(404, 'Draft not found.')
        return self.public(row)

    def save_draft(self, draft_id: str, body: dict, from_ok) -> dict:
        sender = str(body.get('from') or '')
        if sender and not from_ok(sender):
            raise Fail(400, 'You can only send from your own addresses.')
        listed = lambda k: json.dumps(addresses([v for v in body.get(k) or [] if isinstance(v, str)]))
        ref = lambda k: str(body.get(k) or '') if ID.match(str(body.get(k) or '')) else ''
        subject = str(body.get('subject') or '')[:998]
        with self.db() as db:
            row = db.execute('SELECT folder FROM messages WHERE id = ?', (draft_id,)).fetchone()
            if row and row['folder'] != 'drafts':
                raise Fail(409, 'That message has already been sent.')
            db.execute("""INSERT INTO messages (id, thread_id, folder, read, received_at, from_addr, subject, subject_key, text,
                to_json, cc_json, bcc_json, reply_of, forward_of) VALUES (?, ?, 'drafts', 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(id) DO UPDATE SET received_at = excluded.received_at, from_addr = excluded.from_addr,
                subject = excluded.subject, subject_key = excluded.subject_key, text = excluded.text, to_json = excluded.to_json,
                cc_json = excluded.cc_json, bcc_json = excluded.bcc_json, reply_of = excluded.reply_of, forward_of = excluded.forward_of""",
                (draft_id, draft_id, now_ms(), sender, subject, subject_key(subject), str(body.get('text') or '')[:MAX_TEXT],
                 listed('to'), listed('cc'), listed('bcc'), ref('replyOf'), ref('forwardOf')))
        return {'ok': True, 'id': draft_id}

    def delete_draft(self, draft_id: str) -> dict:
        with self.db() as db:
            if not db.execute("DELETE FROM messages WHERE id = ? AND folder = 'drafts'", (draft_id,)).rowcount:
                raise Fail(404, 'Draft not found.')
        return {'ok': True}

    def contacts(self) -> dict:
        counts: dict[str, list] = {}
        with self.db() as db:
            for r in db.execute("SELECT from_addr, from_name, to_json, cc_json, folder FROM messages WHERE folder != 'drafts' ORDER BY received_at DESC LIMIT 5000"):
                sent = r['folder'] == 'sent'
                people = json.loads(r['to_json']) + json.loads(r['cc_json']) if sent else [{'name': r['from_name'], 'address': r['from_addr']}]
                for p in people:
                    entry = counts.setdefault(p['address'].lower(), [p['name'], p['address'], 0])
                    entry[2] += 3 if sent else 1  # people you write to rank higher
                    entry[0] = entry[0] or p['name']
        ranked = sorted(counts.values(), key=lambda e: -e[2])[:300]
        return {'contacts': [{'name': n, 'address': a} for n, a, _ in ranked]}


class Mailer:
    """Sends through the SMTP relay and files a copy under Sent."""

    def __init__(self, store: Store, env: dict, smtp_factory=None):
        self.store = store
        self.senders = [a.strip().lower() for a in env.get('NEXUS_MAIL_FROM', '').split(',') if a.strip()]
        self.name = env.get('NEXUS_MAIL_FROM_NAME', '').strip()
        self.host = env.get('NEXUS_MAIL_SMTP_HOST', 'smtp-relay.brevo.com').strip()
        self.port = int(env.get('NEXUS_MAIL_SMTP_PORT', '587'))
        self.user = env.get('NEXUS_MAIL_SMTP_USER', '').strip()
        self.password = env.get('NEXUS_MAIL_SMTP_PASSWORD', '').strip()
        self.smtp_factory = smtp_factory or (lambda: smtplib.SMTP(self.host, self.port, timeout=60))

    def allowed(self, address: str) -> bool:
        return parseaddr(address)[1].lower() in self.senders

    def config(self) -> dict:
        return {'from': self.senders, 'name': self.name, 'sending': bool(self.user and self.password)}

    def send(self, body: dict) -> dict:
        if not (self.user and self.password):
            raise Fail(503, 'Sending is not set up yet. Add the Brevo SMTP login to the mail service.')
        sender = parseaddr(str(body.get('from') or (self.senders[0] if self.senders else '')))[1].lower()
        if sender not in self.senders:
            raise Fail(400, 'You can only send from your own addresses.')
        to, cc, bcc = (clean_recipients(body.get(k) or []) for k in ('to', 'cc', 'bcc'))
        if not to and not cc and not bcc:
            raise Fail(400, 'Add at least one recipient.')
        if len(to) + len(cc) + len(bcc) > MAX_RECIPIENTS:
            raise Fail(400, f'At most {MAX_RECIPIENTS} recipients per message.')
        subject = str(body.get('subject') or '')
        if re.search(r'[\r\n]', subject) or len(subject) > 998:
            raise Fail(400, 'That subject is not valid.')
        text = str(body.get('text') or '')
        if len(text) > MAX_TEXT:
            raise Fail(400, 'The message is too long.')
        for key in ('replyOf', 'forwardOf'):
            if body.get(key) and not ID.match(str(body[key])):
                raise Fail(400, 'Unknown original message.')

        with self.store.db() as db:
            parent = db.execute('SELECT * FROM messages WHERE id = ?', (body['replyOf'],)).fetchone() if body.get('replyOf') else None
            original = db.execute('SELECT * FROM messages WHERE id = ?', (body['forwardOf'],)).fetchone() if body.get('forwardOf') else None
        if body.get('replyOf') and not parent:
            raise Fail(404, 'The message you are replying to was deleted.')
        if body.get('forwardOf') and not original:
            raise Fail(404, 'The message you are forwarding was deleted.')

        msg = EmailMessage(policy=policy.SMTP)
        msg['From'] = formataddr((self.name, sender)) if self.name else sender
        msg['To'] = ', '.join(formataddr((a['name'], a['address'])) for a in to) if to else 'undisclosed-recipients:;'
        if cc:
            msg['Cc'] = ', '.join(formataddr((a['name'], a['address'])) for a in cc)
        msg['Subject'] = subject
        msg['Date'] = time.strftime('%a, %d %b %Y %H:%M:%S %z')
        message_id = make_msgid(domain=sender.split('@', 1)[1])
        msg['Message-ID'] = message_id
        thread, refs, in_reply_to = None, [], ''
        if parent:
            thread = parent['thread_id']
            refs = (parent['refs'].split() + ([parent['message_id']] if parent['message_id'] else []))[-50:]
            in_reply_to = parent['message_id']
            if in_reply_to:
                msg['In-Reply-To'] = in_reply_to
            if refs:
                msg['References'] = ' '.join(refs)
        msg.set_content(text)

        files: list[tuple[str, str, bytes]] = []
        if original:
            keep = body.get('keepAttachments')
            for i, meta in enumerate(json.loads(original['attachments'])):
                if not isinstance(keep, list) or i in keep:
                    files.append((meta['filename'], meta['type'], self.store.attachment(original['id'], i)[1]))
        for a in body.get('attachments') or []:
            try:
                data = base64.b64decode(str(a.get('data', '')), validate=True)
            except (ValueError, AttributeError):
                raise Fail(400, 'An attachment could not be read.')
            files.append((str(a.get('filename') or 'attachment')[:200], str(a.get('type') or 'application/octet-stream')[:100], data))
        if len(files) > 32:
            raise Fail(400, 'At most 32 attachments per message.')
        for name, mime, data in files:
            main, _, sub = (mime if re.fullmatch(r'[\w.+-]+/[\w.+-]+', mime) else 'application/octet-stream').partition('/')
            msg.add_attachment(data, maintype=main, subtype=sub, filename=name)
        if len(msg.as_bytes()) > 25 * MB:
            raise Fail(413, 'The message is over 25 MB with attachments.')

        recipients = [a['address'] for a in to + cc + bcc]
        try:
            smtp = self.smtp_factory()
        except OSError:
            raise Fail(502, 'Could not reach the mail relay. Check the server can reach it on port 587.')
        try:
            smtp.starttls(context=ssl.create_default_context())
            smtp.login(self.user, self.password)
            smtp.send_message(msg, from_addr=sender, to_addrs=recipients)  # Bcc goes in the envelope only
        except smtplib.SMTPException as e:
            raise Fail(502, f'The mail relay refused the message: {e}')
        except OSError:
            raise Fail(502, 'Lost the connection to the mail relay. The message may not have been sent.')
        finally:
            try:
                smtp.quit()
            except Exception:
                pass

        mid = str(uuid.uuid4())
        meta = self.store.save_attachments(mid, files)
        with self.store.db() as db:
            db.execute("""INSERT INTO messages (id, thread_id, folder, read, received_at, message_id, in_reply_to, refs, subject,
                subject_key, from_addr, from_name, to_json, cc_json, bcc_json, envelope_to, text, attachments)
                VALUES (?, ?, 'sent', 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
                (mid, thread or mid, now_ms(), message_id, in_reply_to, ' '.join(refs), subject, subject_key(subject),
                 sender, self.name, json.dumps(to), json.dumps(cc), json.dumps(bcc), sender, text, json.dumps(meta)))
            if body.get('draftId') and ID.match(str(body['draftId'])):
                db.execute("DELETE FROM messages WHERE id = ? AND folder = 'drafts'", (body['draftId'],))
        return {'ok': True, 'id': mid, 'thread_id': thread or mid}


class Service(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, address, store: Store, mailer: Mailer, token: str):
        super().__init__(address, Handler)
        self.store, self.mailer, self.token = store, mailer, token


class Handler(BaseHTTPRequestHandler):
    server: Service
    protocol_version = 'HTTP/1.1'

    def log_message(self, fmt, *args):  # never log paths with search terms or addresses
        pass

    def reply(self, status: int, body: dict | bytes, headers: dict | None = None) -> None:
        data = body if isinstance(body, bytes) else json.dumps(body).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/octet-stream' if isinstance(body, bytes) else 'application/json')
        self.send_header('Content-Length', str(len(data)))
        self.send_header('Cache-Control', 'no-store')
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(data)

    def body(self, limit: int) -> bytes:
        length = self.headers.get('Content-Length')
        if length is None or not length.isdigit():
            raise Fail(411, 'Content-Length is required.')
        if int(length) > limit:
            raise Fail(413, 'Request too large.')
        return self.rfile.read(int(length))

    def json_body(self, limit: int = MAX_JSON) -> dict:
        try:
            value = json.loads(self.body(limit) or b'{}')
        except ValueError:
            raise Fail(400, 'Send JSON.')
        if not isinstance(value, dict):
            raise Fail(400, 'Send a JSON object.')
        return value

    def authorized(self) -> bool:
        values = self.headers.get_all('Authorization') or []
        expected = ('Bearer ' + self.server.token).encode()
        return (ip_address(self.client_address[0]).is_loopback and len(values) == 1 and len(self.server.token) >= 32
                and hmac.compare_digest(values[0].encode(), expected))

    def handle_any(self, method: str) -> None:
        try:
            if not self.authorized():
                self.close_connection = True
                raise Fail(401, 'Not authorized.')
            url = urlsplit(self.path)
            parts = [p for p in url.path.split('/') if p]
            query = {k: v[0] for k, v in parse_qs(url.query).items()}
            self.route(method, parts, query)
        except Fail as e:
            self.reply(e.status, {'error': str(e)})
        except Exception as e:  # keep the service up; details go to the journal only
            print(f'nexus-mail: {type(e).__name__}: {e}', file=sys.stderr)
            self.reply(500, {'error': 'The mail service hit an error.'})

    def route(self, method: str, parts: list[str], query: dict) -> None:
        store, mailer = self.server.store, self.server.mailer
        match method, parts:
            case 'POST', ['inbound']:
                raw = self.body(MAX_INBOUND)
                self.reply(200, store.receive(raw, self.headers.get('X-Envelope-From', ''), self.headers.get('X-Envelope-To', '')))
            case 'GET', ['threads']:
                offset = query.get('offset', '0')
                if not offset.isdigit():
                    raise Fail(400, 'Bad offset.')
                self.reply(200, store.threads(query.get('folder', 'inbox'), query.get('q', '')[:200], int(offset)))
            case 'GET', ['threads', tid] if ID.match(tid):
                self.reply(200, store.thread(tid))
            case 'POST', ['threads', tid, action] if ID.match(tid):
                self.reply(200, store.act(tid, action))
            case 'GET', ['drafts', did] if ID.match(did):
                self.reply(200, store.get_draft(did))
            case 'PUT', ['drafts', did] if ID.match(did):
                self.reply(200, store.save_draft(did, self.json_body(), mailer.allowed))
            case 'DELETE', ['drafts', did] if ID.match(did):
                self.reply(200, store.delete_draft(did))
            case 'POST', ['send']:
                self.reply(200, mailer.send(self.json_body(MAX_SEND)))
            case 'GET', ['contacts']:
                self.reply(200, store.contacts())
            case 'GET', ['config']:
                self.reply(200, mailer.config())
            case 'GET', ['messages', mid, 'attachments', n] if ID.match(mid) and n.isdigit():
                meta, data = store.attachment(mid, int(n))
                self.reply(200, data, {'Content-Disposition': "attachment; filename*=UTF-8''" + quote(meta['filename'])})
            case _:
                raise Fail(404, 'Not found.')

    def do_GET(self):
        self.handle_any('GET')

    def do_POST(self):
        self.handle_any('POST')

    def do_PUT(self):
        self.handle_any('PUT')

    def do_DELETE(self):
        self.handle_any('DELETE')


def main() -> None:
    env = os.environ
    token = env.get('NEXUS_MAIL_TOKEN', '')
    if len(token) < 32 or re.search(r'\s', token):
        sys.exit('NEXUS_MAIL_TOKEN must be at least 32 characters with no spaces')
    host, _, port = env.get('NEXUS_MAIL_LISTEN', '127.0.0.1:8898').rpartition(':')
    if not ip_address(host).is_loopback:
        sys.exit('NEXUS_MAIL_LISTEN must be a loopback address; the tunnel connects locally')
    store = Store(env.get('NEXUS_MAIL_DIR') or env.get('STATE_DIRECTORY', '/var/lib/nexus-mail'))
    mailer = Mailer(store, dict(env))
    if not mailer.senders:
        sys.exit('NEXUS_MAIL_FROM must list your address, for example liam@liamjc.dev')
    print(f'nexus-mail listening on {host}:{port}', file=sys.stderr)
    Service((host, int(port)), store, mailer, token).serve_forever()


if __name__ == '__main__':
    main()
