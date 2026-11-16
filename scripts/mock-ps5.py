#!/usr/bin/env python3
"""Standalone mock of the PS5 gateway backends (plan U-61).

Same routes and fixtures as gateway/mockps5_test.go; the bodies come from gateway/testdata/ps5.
Listens on 127.0.0.1 only: inference, control and media, one port each.

  MOCK_PS5_TOKEN=<32+ chars> python scripts/mock-ps5.py [--offline] [--busy] [--slow SECONDS] [--error-event]

Stdlib only. The token is read from the environment and is never printed.
"""

import argparse
import base64
import binascii
import copy
import functools
import hmac
import io
import json
import os
import re
import struct
import sys
import threading
import time
import wave
import zlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import NamedTuple
from urllib.parse import urlsplit

FIXTURES = Path(__file__).resolve().parents[1] / "gateway" / "testdata" / "ps5"
HOST = "127.0.0.1"
TOKEN_ENV = "MOCK_PS5_TOKEN"
MIN_TOKEN_LEN = 32
DEFAULT_PORTS = {"inference": 18081, "control": 18082, "media": 18083}
WAV_PLACEHOLDER = b"__WAV_B64__"


class Switches:
    """Fault toggles; see the flags in parse_args."""

    def __init__(self, offline=False, busy=False, slow=0.0, error_event=False):
        self.offline = offline  # every route answers 503 (media /health answers its unavailable body)
        self.busy = busy  # generation and control-mutation routes answer 409
        self.slow = slow  # seconds before each response
        self.error_event = error_event  # streams end with an error event in place of the last event


class Errs(NamedTuple):
    public: bool  # skip the bearer check
    offline: str
    busy: str | None
    unauthorized: str | None


PSLLAMA_ERRS = Errs(False, "error_offline.json", "error_busy.json", "error_unauthorized.json")
CONTROL_ERRS = Errs(False, "control_error_offline.json", "control_error_busy.json", "control_error_unauthorized.json")
HEALTH_ERRS = Errs(True, "media_health_unavailable.json", None, None)


def load_token(environ):
    token = environ.get(TOKEN_ENV, "")
    if len(token) < MIN_TOKEN_LEN:  # the message names the variable, never its value
        raise SystemExit(f"{TOKEN_ENV} must be set to at least {MIN_TOKEN_LEN} characters; refusing to start")
    return token


# Fixtures ---------------------------------------------------------------------------------------

@functools.lru_cache(maxsize=1)
def wav_bytes():
    """10 ms of 16 kHz mono PCM16 silence."""
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(16000)
        w.writeframes(b"\x00" * 320)
    return buf.getvalue()


def is_wav(raw):
    return len(raw) >= 12 and raw[:4] == b"RIFF" and raw[8:12] == b"WAVE"


def read_fixture(name):
    """A fixture's bytes, with __WAV_B64__ replaced by a generated WAV (base64)."""
    raw = (FIXTURES / name).read_bytes()
    return raw.replace(WAV_PLACEHOLDER, base64.b64encode(wav_bytes()))


def patch_fixture(name, values):
    doc = json.loads(read_fixture(name))
    doc.update(values)
    return json.dumps(doc).encode()


def compact(obj):
    return json.dumps(obj, separators=(",", ":"), ensure_ascii=False)


def has_model(model_id):
    doc = json.loads(read_fixture("control_models.json"))
    return any(m["id"] == model_id and m["fits"] for m in doc["models"])


def png_bytes(w, h):
    """w x h RGBA gradient, as the Go mock draws it."""
    rows = bytearray()
    for y in range(h):
        rows.append(0)  # filter type: none
        for x in range(w):
            rows += bytes((x & 0xFF, y & 0xFF, 0x80, 0xFF))

    def chunk(tag, data):
        return struct.pack(">I", len(data)) + tag + data + struct.pack(">I", zlib.crc32(tag + data))

    ihdr = struct.pack(">IIBBBBB", w, h, 8, 6, 0, 0, 0)  # 8-bit RGBA
    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", ihdr) + chunk(b"IDAT", zlib.compress(bytes(rows)))
            + chunk(b"IEND", b""))


# State ------------------------------------------------------------------------------------------

class State:
    def __init__(self, token, sw):
        self.token = token
        self.sw = sw
        self.engine = "llm"
        self.lock = threading.Lock()

    def switches(self):
        with self.lock:
            return copy.copy(self.sw)

    def get_engine(self):
        with self.lock:
            return self.engine

    def set_engine(self, name):
        with self.lock:
            self.engine = name  # the real switch completes later; the mock flips at once


# Handler ----------------------------------------------------------------------------------------

class Handler(BaseHTTPRequestHandler):
    server_version = "MockPS5"
    sys_version = ""

    @property
    def state(self):
        return self.server.state

    def log_message(self, fmt, *args):
        if self.server.verbose:  # request line only; headers (and the token) are never logged
            sys.stderr.write("mock-ps5 %s\n" % (fmt % args))

    def do_GET(self):
        self._route("GET")

    def do_POST(self):
        self._route("POST")

    def _route(self, method):
        length = int(self.headers.get("Content-Length") or 0)
        self.body = self.rfile.read(length) if length > 0 else b""
        path = urlsplit(self.path).path
        for m, pattern, fn in self.server.routes:
            hit = pattern.fullmatch(path) if m == method else None
            if hit:
                fn(self, hit)
                return
        self.send_error(404)

    def json_body(self):
        try:
            return json.loads(self.body)
        except ValueError:
            return None

    def bearer_ok(self):
        want = f"Bearer {self.state.token}".encode()
        return hmac.compare_digest(self.headers.get("Authorization", "").encode(), want)

    def admit(self, errs, busy=False):
        """Applies slow, offline, bearer and busy checks in that order. Writes the failure and returns False if refused."""
        sw = self.state.switches()
        if sw.slow > 0:
            time.sleep(sw.slow)
        if sw.offline:
            self.reply(503, errs.offline)
            return False
        if not errs.public and not self.bearer_ok():
            self.reply(401, errs.unauthorized)
            return False
        if busy and sw.busy:
            self.reply(409, errs.busy)
            return False
        return True

    def reply(self, code, body, ctype="application/json"):
        if isinstance(body, str):
            body = read_fixture(body)
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def sse(self, events, err_event, inject):
        """Writes events as SSE data lines then [DONE]. With inject, the last event is replaced by err_event."""
        if inject and events:
            events = events[:-1]
        frames = [compact(e) for e in events]
        if inject:
            frames.append(compact(err_event))
        frames.append("[DONE]")
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        for data in frames:
            self.wfile.write(f"data: {data}\n\n".encode())
            self.wfile.flush()


def wav_request(h):
    """True when the JSON body's audio_b64 decodes to a WAV."""
    req = h.json_body()
    if not isinstance(req, dict) or not isinstance(req.get("audio_b64"), str):
        return False
    try:
        raw = base64.b64decode(req["audio_b64"], validate=True)
    except (binascii.Error, ValueError):
        return False
    return is_wav(raw)


# Inference (:18081, llama-server) ---------------------------------------------------------------

def inf_models(h, _):
    if h.admit(PSLLAMA_ERRS):
        h.reply(200, "inference_models.json")


def inf_chat(h, _):
    if not h.admit(PSLLAMA_ERRS, busy=True):
        return
    req = h.json_body()
    if not isinstance(req, dict) or not isinstance(req.get("messages"), list) or not req["messages"]:
        h.reply(422, "error_invalid.json")
        return
    if not req.get("stream"):
        h.reply(200, "inference_chat.json")
        return
    sw = h.state.switches()
    events = json.loads(read_fixture("inference_chat_stream.json"))
    h.sse(events, json.loads(read_fixture("inference_error_event.json")), sw.error_event)


# Control (:18082, native/nexus_control.cpp) -----------------------------------------------------

def ctl_status(h, _):
    if h.admit(CONTROL_ERRS):
        h.reply(200, patch_fixture("control_status.json", {"engine": h.state.get_engine()}))


def ctl_models(h, _):
    if h.admit(CONTROL_ERRS):
        h.reply(200, "control_models.json")


def ctl_unload(h, _):
    if h.admit(CONTROL_ERRS, busy=True):
        h.reply(202, "control_accepted.json")


def ctl_load(h, m):
    if not h.admit(CONTROL_ERRS, busy=True):
        return
    if not has_model(m.group(1)):
        h.reply(404, "control_unknown_model.json")
        return
    h.reply(202, "control_accepted.json")


def ctl_engine(h, m):
    if not h.admit(CONTROL_ERRS, busy=True):
        return
    h.state.set_engine(m.group(1))
    h.reply(202, "control_accepted.json")


# Media (:18083) ---------------------------------------------------------------------------------

def media_health(h, _):
    if h.admit(HEALTH_ERRS):
        h.reply(200, "media_health.json")


def media_image(h, _):
    if not h.admit(PSLLAMA_ERRS, busy=True):
        return
    req = h.json_body()
    if not isinstance(req, dict) or not isinstance(req.get("prompt"), str) or not req["prompt"]:
        h.reply(422, "error_invalid.json")
        return
    tpl = json.loads(read_fixture("media_image.json"))
    png = base64.b64encode(png_bytes(tpl["width"], tpl["height"])).decode()
    h.reply(200, patch_fixture("media_image.json", {"data": [{"b64_json": png}]}))


def media_video(h, _):
    if not h.admit(PSLLAMA_ERRS, busy=True):
        return
    req = h.json_body()
    if not isinstance(req, dict) or not isinstance(req.get("prompt"), str) or not req["prompt"]:
        h.reply(422, "error_invalid.json")
        return
    tpl = json.loads(read_fixture("media_video.json"))
    frame = base64.b64encode(png_bytes(tpl["width"], tpl["height"])).decode()
    frames = [{"b64_png": frame} for _ in range(tpl["frames"])]
    h.reply(200, patch_fixture("media_video.json", {"data": frames}))


def media_transcribe(h, _):
    if not h.admit(PSLLAMA_ERRS, busy=True):
        return
    if not wav_request(h):
        h.reply(422, "error_invalid.json")
        return
    h.reply(200, "media_transcription.json")


def media_speech(h, _):
    if not h.admit(PSLLAMA_ERRS, busy=True):
        return
    req = h.json_body()
    if not isinstance(req, dict) or not isinstance(req.get("input"), str) or not req["input"]:
        h.reply(422, "error_invalid.json")
        return
    if req.get("response_format") == "wav":
        h.reply(200, wav_bytes(), ctype="audio/wav")
        return
    h.reply(200, "media_speech.json")


def media_voice(h, _):
    if not h.admit(PSLLAMA_ERRS, busy=True):
        return
    if not wav_request(h):
        h.reply(422, "error_invalid.json")
        return
    sw = h.state.switches()
    events = json.loads(read_fixture("media_voice_events.json"))
    h.sse(events, json.loads(read_fixture("media_voice_error_event.json")), sw.error_event)


INFERENCE_ROUTES = [
    ("GET", re.compile(r"/v1/models"), inf_models),
    ("POST", re.compile(r"/v1/chat/completions"), inf_chat),
]
CONTROL_ROUTES = [
    ("GET", re.compile(r"/v1/nexus/status"), ctl_status),
    ("GET", re.compile(r"/v1/nexus/models"), ctl_models),
    ("POST", re.compile(r"/v1/nexus/models/unload"), ctl_unload),
    ("POST", re.compile(r"/v1/nexus/models/([^/]+)/load"), ctl_load),
    ("POST", re.compile(r"/v1/nexus/engine/(llm|media)"), ctl_engine),
]
MEDIA_ROUTES = [
    ("GET", re.compile(r"/health"), media_health),
    ("POST", re.compile(r"/v1/images/generations"), media_image),
    ("POST", re.compile(r"/v1/videos/generations"), media_video),
    ("POST", re.compile(r"/v1/audio/transcriptions"), media_transcribe),
    ("POST", re.compile(r"/v1/audio/speech"), media_speech),
    ("POST", re.compile(r"/v1/voice/chat"), media_voice),
]


# Servers ----------------------------------------------------------------------------------------

class Server(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = False  # on Windows SO_REUSEADDR lets another process take the port

    def __init__(self, addr, routes, state, verbose):
        self.routes = routes
        self.state = state
        self.verbose = verbose
        super().__init__(addr, Handler)


class Mock:
    """The three listeners. ports=None uses DEFAULT_PORTS; port 0 picks an ephemeral port."""

    def __init__(self, token, sw=None, ports=None, verbose=False):
        ports = ports or DEFAULT_PORTS
        self.state = State(token, sw or Switches())
        self.servers = {}
        self._started = False
        try:
            for name, routes in (("inference", INFERENCE_ROUTES), ("control", CONTROL_ROUTES), ("media", MEDIA_ROUTES)):
                self.servers[name] = Server((HOST, ports[name]), routes, self.state, verbose)
        except OSError:
            self.stop()
            raise

    def url(self, name):
        return f"http://{HOST}:{self.servers[name].server_address[1]}"

    def start(self):
        for srv in self.servers.values():
            threading.Thread(target=srv.serve_forever, daemon=True).start()
        self._started = True

    def stop(self):
        if self._started:
            for srv in self.servers.values():
                srv.shutdown()
            self._started = False
        for srv in self.servers.values():
            srv.server_close()

    def set(self, **changes):
        with self.state.lock:
            for key, value in changes.items():
                if key not in vars(self.state.sw):
                    raise KeyError(key)
                setattr(self.state.sw, key, value)


def nonneg_float(text):
    value = float(text)
    if value < 0:
        raise argparse.ArgumentTypeError("must be >= 0")
    return value


def port_number(text):
    value = int(text)
    if not 1 <= value <= 65535:
        raise argparse.ArgumentTypeError("must be 1-65535")
    return value


def parse_args(argv=None):
    ap = argparse.ArgumentParser(description="Standalone mock PS5 on 127.0.0.1 (inference, control, media).")
    ap.add_argument("--inference-port", type=port_number, default=DEFAULT_PORTS["inference"])
    ap.add_argument("--control-port", type=port_number, default=DEFAULT_PORTS["control"])
    ap.add_argument("--media-port", type=port_number, default=DEFAULT_PORTS["media"])
    ap.add_argument("--offline", action="store_true", help="answer every route 503")
    ap.add_argument("--busy", action="store_true", help="answer generation and control mutations 409")
    ap.add_argument("--slow", type=nonneg_float, default=0.0, metavar="SECONDS", help="delay each response")
    ap.add_argument("--error-event", action="store_true", help="end streams with an error event")
    return ap.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)
    token = load_token(os.environ)
    sw = Switches(offline=args.offline, busy=args.busy, slow=args.slow, error_event=args.error_event)
    ports = {"inference": args.inference_port, "control": args.control_port, "media": args.media_port}
    mock = Mock(token, sw=sw, ports=ports, verbose=True)
    mock.start()
    print(f"mock PS5 on {HOST}: inference {ports['inference']}, control {ports['control']}, "
          f"media {ports['media']} (Ctrl+C to stop)", flush=True)
    try:
        while True:
            time.sleep(1)
    except KeyboardInterrupt:
        pass
    finally:
        mock.stop()
    return 0


if __name__ == "__main__":
    sys.exit(main())
