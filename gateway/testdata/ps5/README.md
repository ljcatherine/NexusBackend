# PS5 mock fixtures

Used by `gateway/mockps5_test.go` (`newMockPS5`). Shapes come from `docs/PSLLAMA-INTEGRATION-PLAN.md` §1.3
(PSllama, `[PSL]` at commit `d079717184d8`) and `native/nexus_control.cpp` (control API). Values are fake.

Fixture files set `"__WAV_B64__"` where the mock inserts a generated 10 ms WAV (base64). PNG frames are generated at
run time with `image/png`, so no PNG bytes live in the fixtures.

## Needs Verification (NV)

These fields or values were not confirmed from the code the plan cites. The mock uses the value shown; change the
fixture when the real PS5 shows otherwise.

- `inference_chat*.json`: `id`, `created`, `object` on chunks; `timings` object (empty here); `usage` keys beyond
  `prompt_tokens`/`completion_tokens`/`total_tokens`; the first delta carrying `role`.
- `inference_models.json`: `owned_by` ("psllama"); `name`; `size_bytes`.
- `inference_error_event.json`, `media_voice_error_event.json`: exact error object for a failed stream. The plan
  confirms "error JSON plus `http_status`" (`[PSL] app/http_server.cpp:1094-1104`) and the voice form
  `{"type":"error","message":…}`; the `error` object shape and `http_status: 500` are assumed.
- `error_busy.json`, `error_offline.json`, `error_invalid.json`: messages. Only the `{"error":{"message","type"}}`
  envelope and the types `invalid_request_error`, `authentication_error`, `server_error` are confirmed. PSllama
  defines no 409 or 503 body; the mock uses 409 for busy and 503 for offline by assumption.
- `error_unauthorized.json`: message text is confirmed (`[PSL] app/http_server.cpp:1959-1964`).
- `media_health*.json`: `model_loaded`, `model_file`, `image_model`, `stt_model`, `tts_model` values and `parameters`
  contents (plan lists the keys only). Whether `/health` requires the bearer is NV; the mock does not require it.
- `media_image.json`, `media_video.json`, `media_speech.json`, `media_transcription.json`: `timings` values, `seed`,
  `cfg_scale`, `steps`, `fps`, `sample_rate`, `frames` (speech), `language`, `segments` values. Keys are from the plan.
- `media_voice_events.json`: `{"status":"loading"}` event; `transcript`, `text`, `audio`, `done` event keys are from
  the plan; the `tts_s`, `at_s` and `timings` values are assumed; the `done` event's `timings` contents are NV.
- Whether `:8081` and `:8083` use the same bearer as `:8082`: NV. The mock uses one fake token for all three.
- `control_status.json`: `state`, `model`, `memory_*`, `kv_cache_gib`, `generation_tps` values are fake. Keys are from
  `nexus_control.cpp:450-466`. `load_progress: null` is one of the cases the code emits.
- `control_models.json`: `drive` ("app0"), `architecture`, and `quantization_type` numbers are NV. `id` is 16 hex
  digits as `model_id()` produces (`nexus_control.cpp:202-208`); the actual IDs are FNV-1a hashes of the model path.
- `control_error_offline.json`: body NV (the C++ has no offline response).
- `control_accepted.json`: `202 {"status":"switching","restart_required":true}` confirmed (`nexus_control.cpp:522,540`).
  The mock flips the engine at once; the real switch completes later and is not modelled.
- `control_error_busy.json`: `"inference is active"` confirmed for 409 (`nexus_control.cpp:515,539`); the
  `"model switch already pending"` case is not modelled.
