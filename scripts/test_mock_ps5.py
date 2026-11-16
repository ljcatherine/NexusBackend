import base64
import contextlib
import importlib.util
import io
import json
import struct
import time
import unittest
import urllib.error
import urllib.request
import wave
from pathlib import Path


SCRIPT = Path(__file__).with_name("mock-ps5.py")  # dash in the name, so load by path
_spec = importlib.util.spec_from_file_location("mock_ps5", SCRIPT)
mock_ps5 = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(mock_ps5)

TOKEN = "test-token-0123456789abcdef0123456789abcdef"
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))  # never send loopback through a proxy
CHAT = json.dumps({"messages": [{"role": "user", "content": "hi"}]})
STREAM = json.dumps({"stream": True, "messages": [{"role": "user", "content": "hi"}]})
WAV_B64 = base64.b64encode(mock_ps5.wav_bytes()).decode()


def call(base, method, path, body=None, bearer=TOKEN):
    data = body.encode() if isinstance(body, str) else body
    req = urllib.request.Request(base + path, data=data, method=method)
    if bearer is not None:
        req.add_header("Authorization", f"Bearer {bearer}")
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with OPENER.open(req, timeout=10) as resp:
            return resp.status, resp.headers, resp.read()
    except urllib.error.HTTPError as err:
        with err:
            return err.code, err.headers, err.read()


def png_size(raw):
    assert raw[:8] == b"\x89PNG\r\n\x1a\n", "not a PNG"
    return struct.unpack(">II", raw[16:24])


class MockPS5Test(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.mock = mock_ps5.Mock(TOKEN, ports={"inference": 0, "control": 0, "media": 0})
        cls.mock.start()
        cls.inf = cls.mock.url("inference")
        cls.ctl = cls.mock.url("control")
        cls.med = cls.mock.url("media")

    @classmethod
    def tearDownClass(cls):
        cls.mock.stop()

    def setUp(self):
        self.mock.set(offline=False, busy=False, slow=0.0, error_event=False)

    def test_token_is_required_and_long_enough(self):
        with self.assertRaises(SystemExit):
            mock_ps5.load_token({})
        with self.assertRaises(SystemExit) as cm:
            mock_ps5.load_token({"MOCK_PS5_TOKEN": "short-secret-value"})
        self.assertNotIn("short-secret-value", str(cm.exception))
        self.assertEqual(mock_ps5.load_token({"MOCK_PS5_TOKEN": TOKEN}), TOKEN)

    def test_listeners_bind_loopback_only(self):
        for srv in self.mock.servers.values():
            self.assertEqual(srv.server_address[0], "127.0.0.1")

    def test_cli_flags_and_ports(self):
        args = mock_ps5.parse_args(["--offline", "--busy", "--slow", "0.5", "--error-event", "--media-port", "19000"])
        self.assertTrue(args.offline and args.busy and args.error_event)
        self.assertEqual(args.slow, 0.5)
        self.assertEqual(args.media_port, 19000)
        self.assertEqual(args.inference_port, 18081)
        for bad in (["--slow", "-1"], ["--control-port", "70000"]):
            with self.subTest(bad=bad), self.assertRaises(SystemExit), contextlib.redirect_stderr(io.StringIO()):
                mock_ps5.parse_args(bad)

    def test_fixtures_and_generators(self):
        for fixture in (mock_ps5.FIXTURES).glob("*.json"):
            with self.subTest(fixture=fixture.name):
                json.loads(mock_ps5.read_fixture(fixture.name))
        self.assertNotIn(b"__WAV_B64__", mock_ps5.read_fixture("media_speech.json"))
        self.assertTrue(mock_ps5.is_wav(mock_ps5.wav_bytes()))
        with wave.open(io.BytesIO(mock_ps5.wav_bytes())) as w:
            self.assertEqual((w.getnchannels(), w.getsampwidth(), w.getframerate(), w.getnframes()), (1, 2, 16000, 160))
        self.assertEqual(png_size(mock_ps5.png_bytes(3, 2)), (3, 2))

    def test_inference_routes(self):
        status, _, body = call(self.inf, "GET", "/v1/models")
        self.assertEqual(status, 200)
        json.loads(body)
        status, _, body = call(self.inf, "POST", "/v1/chat/completions", CHAT)
        self.assertEqual(status, 200)
        self.assertIn(b"choices", body)
        status, headers, body = call(self.inf, "POST", "/v1/chat/completions", STREAM)
        self.assertEqual(status, 200)
        self.assertEqual(headers["Content-Type"], "text/event-stream")
        self.assertTrue(body.endswith(b"data: [DONE]\n\n"))
        self.assertIn(b'"finish_reason":"stop"', body)
        status, _, _ = call(self.inf, "POST", "/v1/chat/completions", '{"messages":[]}')
        self.assertEqual(status, 422)

    def test_control_routes(self):
        status, _, body = call(self.ctl, "GET", "/v1/nexus/status")
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(body)["engine"], "llm")
        self.assertEqual(call(self.ctl, "GET", "/v1/nexus/models")[0], 200)
        self.assertEqual(call(self.ctl, "POST", "/v1/nexus/models/0123456789abcdef/load")[0], 202)
        self.assertEqual(call(self.ctl, "POST", "/v1/nexus/models/fedcba9876543210/load")[0], 404)
        self.assertEqual(call(self.ctl, "POST", "/v1/nexus/models/unload")[0], 202)
        self.assertEqual(call(self.ctl, "POST", "/v1/nexus/engine/media")[0], 202)
        _, _, body = call(self.ctl, "GET", "/v1/nexus/status")
        self.assertEqual(json.loads(body)["engine"], "media")
        self.assertEqual(call(self.ctl, "POST", "/v1/nexus/engine/llm")[0], 202)
        self.assertEqual(call(self.ctl, "POST", "/v1/nexus/engine/bogus")[0], 404)

    def test_media_routes(self):
        status, _, body = call(self.med, "GET", "/health", bearer=None)
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(body)["status"], "ok")

        status, _, body = call(self.med, "POST", "/v1/images/generations", '{"prompt":"a cat"}')
        self.assertEqual(status, 200)
        image = json.loads(body)["data"][0]["b64_json"]
        self.assertEqual(png_size(base64.b64decode(image)), (256, 256))

        status, _, body = call(self.med, "POST", "/v1/videos/generations", '{"prompt":"a cat"}')
        self.assertEqual(status, 200)
        video = json.loads(body)
        self.assertEqual(len(video["data"]), video["frames"])
        for frame in video["data"]:
            self.assertEqual(png_size(base64.b64decode(frame["b64_png"])), (128, 128))

        self.assertEqual(call(self.med, "POST", "/v1/audio/transcriptions", json.dumps({"audio_b64": WAV_B64}))[0], 200)
        self.assertEqual(call(self.med, "POST", "/v1/audio/transcriptions", '{"audio_b64":"bm90IGF1ZGlv"}')[0], 422)

        self.assertEqual(call(self.med, "POST", "/v1/audio/speech", '{"input":"hello"}')[0], 200)
        status, headers, body = call(self.med, "POST", "/v1/audio/speech", '{"input":"hello","response_format":"wav"}')
        self.assertEqual(status, 200)
        self.assertEqual(headers["Content-Type"], "audio/wav")
        self.assertTrue(mock_ps5.is_wav(body))

        status, _, body = call(self.med, "POST", "/v1/voice/chat", json.dumps({"audio_b64": WAV_B64}))
        self.assertEqual(status, 200)
        self.assertIn(b'"type":"done"', body)
        self.assertTrue(body.endswith(b"data: [DONE]\n\n"))

    def test_bearer_required_except_media_health(self):
        checks = [
            (self.inf, "GET", "/v1/models", None),
            (self.inf, "POST", "/v1/chat/completions", CHAT),
            (self.ctl, "GET", "/v1/nexus/status", None),
            (self.ctl, "GET", "/v1/nexus/models", None),
            (self.ctl, "POST", "/v1/nexus/models/unload", None),
            (self.ctl, "POST", "/v1/nexus/models/0123456789abcdef/load", None),
            (self.ctl, "POST", "/v1/nexus/engine/llm", None),
            (self.med, "POST", "/v1/images/generations", '{"prompt":"a cat"}'),
            (self.med, "POST", "/v1/videos/generations", '{"prompt":"a cat"}'),
            (self.med, "POST", "/v1/audio/transcriptions", json.dumps({"audio_b64": WAV_B64})),
            (self.med, "POST", "/v1/audio/speech", '{"input":"hello"}'),
            (self.med, "POST", "/v1/voice/chat", json.dumps({"audio_b64": WAV_B64})),
        ]
        for base, method, path, body in checks:
            with self.subTest(path=path):
                self.assertEqual(call(base, method, path, body, bearer=None)[0], 401)
                self.assertEqual(call(base, method, path, body, bearer="wrong-token-value")[0], 401)

    def test_switch_offline(self):
        self.mock.set(offline=True)
        self.assertEqual(call(self.inf, "POST", "/v1/chat/completions", CHAT)[0], 503)
        self.assertEqual(call(self.ctl, "GET", "/v1/nexus/status")[0], 503)
        status, _, body = call(self.med, "GET", "/health", bearer=None)
        self.assertEqual(status, 503)
        self.assertEqual(json.loads(body)["status"], "unavailable")

    def test_switch_busy(self):
        self.mock.set(busy=True)
        self.assertEqual(call(self.med, "POST", "/v1/images/generations", '{"prompt":"a cat"}')[0], 409)
        self.assertEqual(call(self.ctl, "POST", "/v1/nexus/models/0123456789abcdef/load")[0], 409)
        self.assertEqual(call(self.ctl, "GET", "/v1/nexus/status")[0], 200)

    def test_switch_slow(self):
        self.mock.set(slow=0.1)
        start = time.monotonic()
        self.assertEqual(call(self.med, "GET", "/health", bearer=None)[0], 200)
        self.assertGreaterEqual(time.monotonic() - start, 0.09)

    def test_switch_error_event(self):
        self.mock.set(error_event=True)
        status, _, body = call(self.inf, "POST", "/v1/chat/completions", STREAM)
        self.assertEqual(status, 200)
        self.assertIn(b'"http_status":500', body)
        self.assertNotIn(b'"finish_reason":"stop"', body)  # the last event was replaced
        self.assertTrue(body.endswith(b"data: [DONE]\n\n"))
        status, _, body = call(self.med, "POST", "/v1/voice/chat", json.dumps({"audio_b64": WAV_B64}))
        self.assertIn(b'"type":"error"', body)
        self.assertNotIn(b'"type":"done"', body)

    def test_initial_switches_apply(self):
        mock = mock_ps5.Mock(TOKEN, sw=mock_ps5.Switches(offline=True), ports={"inference": 0, "control": 0, "media": 0})
        mock.start()
        try:
            self.assertEqual(call(mock.url("inference"), "GET", "/v1/models")[0], 503)
        finally:
            mock.stop()


if __name__ == "__main__":
    unittest.main()
