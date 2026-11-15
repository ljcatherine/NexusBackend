import json
import os
import subprocess
import sys
import unittest
from unittest.mock import patch

from dsh_engine import DSH_VERSION, DshEngine, dsh_worker
from hermes_engine import HermesUnavailable, REVISION
from test_hermes_engine import LoopFixture, MESSAGES, WORKSPACE
from test_server import FakeController, api

IMAGE = 'sha256:' + 'b' * 64
DSH_LABELS = json.dumps({'org.nexus.dsh.version': DSH_VERSION, 'org.nexus.dsh.protocol': '1'}).encode()


class DshEngineTests(unittest.TestCase):
    def setUp(self):
        self.controller = FakeController()
        self.controller.context_limit_override = None
        self.controller.reported_context = 65536
        self.worker = LoopFixture()
        self.engine = DshEngine(self.worker)

    def run_engine(self, **kwargs):
        state = dict(combined=[], prompt='Inspect', agents=[], workspace_context_bytes=0,
            model='ps5', output_budget=4096, context_window=65536, workspace=WORKSPACE, engine='dsh')
        return self.engine.run(self.controller, MESSAGES, 'ps5', 65536, 4096, WORKSPACE,
            lambda _: None, lambda: False, state, kwargs.pop('proposal_checkpoint', None), **kwargs)

    def test_controller_selects_dsh_and_all_inference_uses_ps5_transport(self):
        self.controller.dsh_engine = self.engine
        self.controller.completion_outputs = ['{"action":"read_file","path":"src/main.py"}',
                                               '{"action":"answer","response":"Inspected with DSH"}']
        result = self.controller.run({'prompt': 'Inspect', 'workspace': WORKSPACE, 'engine': 'dsh'})
        self.assertEqual(result['response'], 'Inspected with DSH')
        self.assertEqual(result['workspace_tools'][0]['tool'], 'read_file')
        self.assertEqual(self.controller.last_completion_payload['model'], self.controller.model)
        names = {tool['function']['name'] for tool in self.worker.request['tools']}
        self.assertEqual(names, {'search', 'read_file'})

    def test_dsh_never_gets_proposals_or_resume(self):
        def checkpoint(_state):
            self.fail('DSH must not reach a proposal checkpoint')
        self.controller.completion_outputs = ['{"action":"propose_patch","path":"main.py","replacement":"x"}']
        with self.assertRaises(HermesUnavailable):
            self.run_engine(proposal_checkpoint=checkpoint)
        self.assertNotIn('propose_patch', {t['function']['name'] for t in self.worker.request['tools']})
        with self.assertRaises(HermesUnavailable):
            self.run_engine(resume={'state': {}, 'step': 0, 'outcome': {}})
        self.assertEqual(self.controller.task_payloads, [])

    def test_validation_fails_closed(self):
        with self.assertRaises(api.AgentError) as raised:
            self.controller.validate_run({'prompt': 'x', 'engine': 'dsh'})
        self.assertEqual(raised.exception.status, 503)
        self.controller.dsh_engine = self.engine
        self.controller.validate_run({'prompt': 'x', 'engine': 'dsh'})
        with self.assertRaises(api.AgentError) as raised:
            self.controller.validate_run({'prompt': 'x', 'engine': 'dsh', 'workspace': WORKSPACE,
                                          'allow_proposals': True})
        self.assertEqual(raised.exception.status, 400)
        for bad in ('DSH', 'deepseek', ['dsh']):
            with self.assertRaises(api.AgentError): self.controller.validate_run({'prompt': 'x', 'engine': bad})

    def test_small_context_is_refused_before_any_inference(self):
        self.controller.dsh_engine = self.engine
        self.controller.reported_context = 32768
        with self.assertRaises(api.AgentError) as raised:
            self.controller.run({'prompt': 'Inspect', 'workspace': WORKSPACE, 'engine': 'dsh'})
        self.assertEqual(raised.exception.status, 413)
        self.assertIsNone(self.controller.last_completion_payload)
        self.assertFalse(hasattr(self.worker, 'request'))

    def test_worker_container_is_isolated_and_checks_dsh_labels(self):
        worker = dsh_worker(sys.executable, IMAGE)
        created = []
        def run(command, **_kwargs):
            if command[1:3] == ['image', 'inspect']:
                return subprocess.CompletedProcess(command, 0, DSH_LABELS)
            if command[1] == 'ps':
                self.assertIn('name=nexus-dsh-', command)
                return subprocess.CompletedProcess(command, 0, b'')
            if command[1] == 'create':
                created.append(command)
                raise subprocess.TimeoutExpired(command, 15)
            return subprocess.CompletedProcess(command, 0)
        with patch('hermes_engine.subprocess.run', side_effect=run):
            with self.assertRaises(HermesUnavailable):
                worker.converse({}, lambda _: None, lambda: False)
        command = created[0]
        for flag, value in (('--network', 'none'), ('--pids-limit', '256'), ('--user', '65534:65534'),
                            ('--cap-drop', 'ALL'), ('--entrypoint', '/usr/local/bin/node')):
            self.assertEqual(command[command.index(flag) + 1], value)
        self.assertIn('--read-only', command)
        self.assertTrue(command[command.index('--name') + 1].startswith('nexus-dsh-'))
        self.assertEqual(command[-2:], [IMAGE, '/opt/nexus/worker.mjs'])
        self.assertTrue(worker.quarantined)

    def test_hermes_image_is_not_accepted_as_dsh(self):
        worker = dsh_worker(sys.executable, IMAGE)
        hermes = json.dumps({'org.nexus.hermes.revision': REVISION, 'org.nexus.hermes.protocol': '1'}).encode()
        with patch('hermes_engine.subprocess.run', return_value=subprocess.CompletedProcess([], 0, hermes)), \
                patch('hermes_engine.subprocess.Popen') as launch:
            with self.assertRaisesRegex(HermesUnavailable, 'DeepSeek Harness image'):
                worker.converse({}, lambda _: None, lambda: False)
            launch.assert_not_called()

    @unittest.skipUnless(os.environ.get('NEXUS_DSH_TEST_IMAGE'), 'requires locally built pinned DSH image')
    def test_real_dsh_loop_in_networkless_container(self):
        self.engine.worker = dsh_worker(os.environ['NEXUS_DSH_TEST_DOCKER'], os.environ['NEXUS_DSH_TEST_IMAGE'])
        self.controller.completion_outputs = ['{"action":"read_file","path":"src/main.py"}',
                                               '{"action":"answer","response":"Verified DSH loop"}']
        result = self.run_engine()
        self.assertEqual(result[0], 'Verified DSH loop')
        self.assertEqual(result[3][0]['tool'], 'read_file')
        self.assertFalse(self.engine.worker.quarantined)

    @unittest.skipUnless(os.environ.get('NEXUS_DSH_TEST_IMAGE'), 'requires locally built pinned DSH image')
    def test_real_container_has_no_egress_mounts_or_root_write_access(self):
        docker, image = os.environ['NEXUS_DSH_TEST_DOCKER'], os.environ['NEXUS_DSH_TEST_IMAGE']
        self.engine.worker = dsh_worker(docker, image)
        self.controller.completion_outputs = ['{"action":"answer","response":"Isolation verified"}']
        inference = self.controller.stream_completion
        probed = []
        def probe(payload, cancelled):
            ids = subprocess.check_output([docker, 'ps', '--filter', 'name=nexus-dsh-', '--format', '{{.ID}}'], text=True).split()
            self.assertEqual(len(ids), 1)
            info = json.loads(subprocess.check_output([docker, 'inspect', ids[0]]))[0]
            host = info['HostConfig']
            self.assertEqual(host['NetworkMode'], 'none')
            self.assertTrue(host['ReadonlyRootfs'])
            self.assertEqual(host['CapDrop'], ['ALL'])
            self.assertIn('no-new-privileges', host['SecurityOpt'])
            self.assertFalse(info['Mounts'])
            code = ("const net=require('net'),fs=require('fs');"
                "const s=net.connect(443,'1.1.1.1');s.setTimeout(300);"
                "const done=network=>{let writable=true;try{fs.writeFileSync('/nexus-write-probe','x')}catch{writable=false}"
                "console.log(JSON.stringify({uid:process.getuid(),network,writable}));process.exit(0)};"
                "s.on('connect',()=>done(true));s.on('error',()=>done(false));s.on('timeout',()=>done(false));")
            result = json.loads(subprocess.check_output([docker, 'exec', ids[0], '/usr/local/bin/node', '-e', code]))
            self.assertEqual(result, {'uid': 65534, 'network': False, 'writable': False})
            probed.append(True)
            return inference(payload, cancelled)
        self.controller.stream_completion = probe
        self.assertEqual(self.run_engine()[0], 'Isolation verified')
        self.assertEqual(probed, [True])


if __name__ == '__main__':
    unittest.main()
