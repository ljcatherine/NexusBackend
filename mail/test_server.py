"""python3 -m unittest mail/test_server.py — Nexus Mail service tests (no network)."""
import base64
import json
import os
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
import uuid

import server

ME = 'liam@liamjc.dev'
TOKEN = 't' * 40


def raw_mail(subject='Hello', sender='Sam Rivera <sam@example.org>', msg_id='<a1@example.org>', extra='', body='Hi Liam', attach=False):
    head = f'From: {sender}\r\nTo: Liam <{ME}>\r\nSubject: {subject}\r\nMessage-ID: {msg_id}\r\n{extra}MIME-Version: 1.0\r\n'
    if not attach:
        return (head + 'Content-Type: text/plain; charset=utf-8\r\n\r\n' + body + '\r\n').encode()
    return (head + 'Content-Type: multipart/mixed; boundary="b"\r\n\r\n--b\r\nContent-Type: text/html; charset=utf-8\r\n\r\n'
            f'<p>{body}</p>\r\n--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename="plan.pdf"\r\n'
            'Content-Transfer-Encoding: base64\r\n\r\n' + base64.b64encode(b'%PDF-1 fake').decode() + '\r\n--b--\r\n').encode()


class FakeSMTP:
    sent = []

    def starttls(self, context=None): self.tls = True
    def login(self, user, password): self.user = user
    def send_message(self, msg, from_addr, to_addrs): FakeSMTP.sent.append((msg, from_addr, to_addrs))
    def quit(self): pass


class MailTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.store = server.Store(self.tmp.name)
        env = {'NEXUS_MAIL_FROM': f'{ME},hello@liamjc.dev', 'NEXUS_MAIL_FROM_NAME': 'Liam',
               'NEXUS_MAIL_SMTP_USER': 'u@smtp-brevo.com', 'NEXUS_MAIL_SMTP_PASSWORD': 'key'}
        self.mailer = server.Mailer(self.store, env, smtp_factory=FakeSMTP)
        FakeSMTP.sent = []

    def tearDown(self):
        self.tmp.cleanup()

    def inbox(self, folder='inbox', q=''):
        return self.store.threads(folder, q, 0)['threads']

    def test_receive_parses_html_and_attachments(self):
        r = self.store.receive(raw_mail(body='Plans <b>attached</b>', attach=True), 'bounce@example.org', ME)
        [t] = self.inbox()
        self.assertEqual((t['from_name'], t['subject'], t['snippet']), ('Sam Rivera', 'Hello', 'Plans attached'))
        self.assertTrue(t['unread'] and t['has_attachments'])
        meta, data = self.store.attachment(r['id'], 0)
        self.assertEqual((meta['filename'], data), ('plan.pdf', b'%PDF-1 fake'))

    def test_redelivery_is_not_duplicated(self):
        a = self.store.receive(raw_mail(), 's@example.org', ME)
        b = self.store.receive(raw_mail(), 's@example.org', ME)
        self.assertEqual(a['id'], b['id'])
        self.assertEqual(len(self.inbox()), 1)

    def test_reply_threads_and_sets_headers(self):
        first = self.store.receive(raw_mail(), 's@example.org', ME)
        sent = self.mailer.send({'from': ME, 'to': ['Sam <sam@example.org>'], 'bcc': ['secret@example.net'],
                                 'subject': 'Re: Hello', 'text': 'Sure!', 'replyOf': first['id']})
        msg, envelope_from, rcpt = FakeSMTP.sent[0]
        self.assertEqual(sent['thread_id'], first['thread_id'])
        self.assertEqual(msg['In-Reply-To'], '<a1@example.org>')
        self.assertEqual(msg['References'], '<a1@example.org>')
        self.assertIsNone(msg['Bcc'])
        self.assertEqual(rcpt, ['sam@example.org', 'secret@example.net'])
        self.assertEqual((envelope_from, msg['From']), (ME, f'Liam <{ME}>'))
        # Their answer references our Message-ID and lands in the same conversation.
        back = self.store.receive(raw_mail('Re: Hello', msg_id='<a2@example.org>', extra=f"In-Reply-To: {msg['Message-ID']}\r\n"), 's@example.org', ME)
        self.assertEqual(back['thread_id'], first['thread_id'])
        [t] = self.inbox()
        self.assertEqual(t['count'], 3)
        self.assertEqual(t['people'], ['Sam Rivera', 'me'])

    def test_reply_without_references_matches_subject_and_person(self):
        self.mailer.send({'from': ME, 'to': ['ada@example.com'], 'subject': 'Engine notes', 'text': 'x'})
        back = self.store.receive(raw_mail('RE: Engine notes', sender='Ada <ada@example.com>', msg_id='<z@x>'), 'ada@example.com', ME)
        stranger = self.store.receive(raw_mail('Re: Engine notes', sender='eve@example.com', msg_id='<y@x>'), 'eve@example.com', ME)
        self.assertEqual(len(self.store.thread(back['thread_id'])['messages']), 2)
        self.assertNotEqual(stranger['thread_id'], back['thread_id'])

    def test_forward_keeps_chosen_attachments(self):
        orig = self.store.receive(raw_mail(attach=True), 's@example.org', ME)
        self.mailer.send({'from': ME, 'to': ['ada@example.com'], 'subject': 'Fwd: Hello', 'text': 'fyi', 'forwardOf': orig['id'],
                          'attachments': [{'filename': 'n.txt', 'type': 'text/plain', 'data': base64.b64encode(b'note').decode()}]})
        msg = FakeSMTP.sent[0][0]
        self.assertEqual([p.get_filename() for p in msg.iter_attachments()], ['plan.pdf', 'n.txt'])
        self.mailer.send({'from': ME, 'to': ['ada@example.com'], 'subject': 'Fwd: Hello', 'text': 'fyi', 'forwardOf': orig['id'], 'keepAttachments': []})
        self.assertEqual(list(FakeSMTP.sent[1][0].iter_attachments()), [])

    def test_send_is_validated(self):
        bad = [({'from': 'ceo@bank.com', 'to': ['a@b.co']}, 400), ({'from': ME, 'to': []}, 400),
               ({'from': ME, 'to': ['not an address']}, 400), ({'from': ME, 'to': ['a@b.co'], 'subject': 'x\r\nBcc: e@v.il'}, 400),
               ({'from': ME, 'to': [f'u{i}@b.co' for i in range(51)]}, 400), ({'from': ME, 'to': ['a@b.co'], 'replyOf': str(uuid.uuid4())}, 404)]
        for body, status in bad:
            with self.assertRaises(server.Fail) as e:
                self.mailer.send(body)
            self.assertEqual(e.exception.status, status, body)
        self.assertEqual(FakeSMTP.sent, [])
        off = server.Mailer(self.store, {'NEXUS_MAIL_FROM': ME}, smtp_factory=FakeSMTP)
        with self.assertRaises(server.Fail) as e:
            off.send({'from': ME, 'to': ['a@b.co']})
        self.assertEqual(e.exception.status, 503)

    def test_folders_trash_and_permanent_delete(self):
        r = self.store.receive(raw_mail(attach=True), 's@example.org', ME)
        tid = r['thread_id']
        self.store.act(tid, 'star')
        self.store.act(tid, 'archive')
        self.assertEqual(self.inbox(), [])
        self.assertEqual(len(self.inbox('all')), 1)
        self.assertEqual(len(self.inbox('starred')), 1)
        self.store.act(tid, 'trash')
        self.assertEqual((self.inbox('all'), self.inbox('starred'), len(self.inbox('trash'))), ([], [], 1))
        self.store.act(tid, 'untrash')
        self.assertEqual(len(self.inbox('all')), 1)
        self.assertEqual(self.store.act(tid, 'delete')['changed'], 0)  # only trashed mail can be deleted
        self.store.act(tid, 'trash')
        self.store.act(tid, 'delete')
        self.assertFalse(os.path.exists(os.path.join(self.tmp.name, 'attachments', r['id'])))
        with self.assertRaises(server.Fail):
            self.store.thread(tid)

    def test_drafts_and_search(self):
        did = str(uuid.uuid4())
        self.store.save_draft(did, {'from': ME, 'to': ['ada@example.com'], 'subject': 'Plan', 'text': 'draft'}, self.mailer.allowed)
        self.assertEqual(self.store.get_draft(did)['to'], [{'name': '', 'address': 'ada@example.com'}])
        self.assertEqual(len(self.inbox('drafts')), 1)
        self.store.receive(raw_mail(msg_id='<count@example.org>'), 's@example.org', ME)
        self.assertEqual(self.store.threads('sent', '', 0)['counts'], {'inbox': 1, 'drafts': 1})
        self.mailer.send({'from': ME, 'to': ['ada@example.com'], 'subject': 'Plan', 'text': 'final', 'draftId': did})
        self.assertEqual(self.inbox('drafts'), [])
        with self.assertRaises(server.Fail) as e:
            self.store.save_draft(self.inbox('sent')[0]['id'], {'subject': 'x'}, self.mailer.allowed)
        self.assertEqual(e.exception.status, 409)
        self.store.receive(raw_mail('Minecraft 50% off_sale'), 's@example.org', ME)
        self.assertEqual(len(self.inbox('inbox', '50%')), 1)
        self.assertEqual(self.inbox('inbox', '50%x'), [])
        self.assertEqual(self.store.contacts()['contacts'][0]['address'], 'ada@example.com')


class HTTPTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        store = server.Store(self.tmp.name)
        self.srv = server.Service(('127.0.0.1', 0), store, server.Mailer(store, {'NEXUS_MAIL_FROM': ME}), TOKEN)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.base = f'http://127.0.0.1:{self.srv.server_address[1]}'

    def tearDown(self):
        self.srv.shutdown()
        self.srv.server_close()
        self.tmp.cleanup()

    def call(self, path, method='GET', data=None, token=TOKEN, headers=None):
        req = urllib.request.Request(self.base + path, data=data, method=method, headers=headers or {})
        if token:
            req.add_header('Authorization', f'Bearer {token}')
        try:
            with urllib.request.urlopen(req) as res:
                return res.status, res.read(), res.headers
        except urllib.error.HTTPError as e:
            with e:
                return e.code, e.read(), e.headers

    def test_token_required(self):
        self.assertEqual(self.call('/threads', token=None)[0], 401)
        self.assertEqual(self.call('/threads', token='x' * 40)[0], 401)
        self.assertEqual(self.call('/threads')[0], 200)

    def test_inbound_then_download_attachment(self):
        status, body, _ = self.call('/inbound', 'POST', raw_mail(attach=True), headers={'X-Envelope-To': ME})
        mid = json.loads(body)['id']
        status, data, headers = self.call(f'/messages/{mid}/attachments/0')
        self.assertEqual((status, data), (200, b'%PDF-1 fake'))
        self.assertIn("filename*=UTF-8''plan.pdf", headers['Content-Disposition'])
        self.assertEqual(self.call('/threads?folder=nope')[0], 400)
        self.assertEqual(self.call('/../etc/passwd')[0], 404)
        self.assertEqual(json.loads(self.call('/config')[1])['sending'], False)


if __name__ == '__main__':
    unittest.main()
