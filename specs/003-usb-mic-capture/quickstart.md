# Quickstart: validating wireless microphone captures

Runnable checks that prove the feature end to end:

- settings: [contracts/configuration.md](contracts/configuration.md)
- protocol: [contracts/mic-http-api.md](contracts/mic-http-api.md)
- messages: [contracts/telegram-ux.md](contracts/telegram-ux.md)

Every host, token and chat ID below is a fake example.

## 0. Gate (every task)

```bash
task lint && golangci-lint fmt --diff && task test:coverage && task build && task build:mic
go list -deps ./cmd/voicenotes | grep -E 'gen2brain/malgo|internal/micaudio' && echo "FENCE BROKEN"   # must print nothing
```

## 1. Hardware verification (once the kit arrives; research R1)

On the host the receiver is plugged into (the Pi runs Ubuntu 26.04, aarch64):

```bash
sudo apt install libasound2t64 evtest alsa-utils
lsusb | grep -i 2ca3                 # expect 2ca3:4011 (another ID: record it, update R1)
arecord -l                           # a capture device for the receiver
sudo evtest                          # pick the receiver's "Consumer Control"; press Linking on the TX
                                     # expect: EV_KEY KEY_VOLUMEUP value 1, then value 0, per tap
arecord -D plughw:<card>,0 -f S16_LE -r 48000 -c 2 -d 5 test.wav && aplay test.wav
```

Save anonymised outputs (`lsusb -v` without serial numbers, the sysfs paths involved, raw evtest
bytes) as fixtures under `internal/mic/testdata/` and `internal/micdev/testdata/`.

Before the kit arrives, the device code is exercised with the laptop's own microphone and a
keyboard volume-up key. Set `MIC_CAPTURE_DEVICE` and `MIC_INPUT_DEVICE` explicitly.

## 2. Service side

Set this on the service:

```text
MIC_UPLOAD_TOKEN=FAKE-0123456789abcdef0123456789abcdef
MIC_NOTIFY_CHAT_ID=-1001234567890
```

Then check from any machine:

| Do | Expect |
|---|---|
| `curl -X PUT http://voicenotes.example.org:8080/mic/v1/recordings/1 -H 'Authorization: Bearer wrong'` | 401; nothing stored |
| Upload a valid 16 kHz mono WAV with a correct token, hash and headers | 201 `stored`; note created, time = `X-Recording-Started-At` |
| The same request again | 200 `duplicate`; still one note |
| Unset `MIC_UPLOAD_TOKEN` and restart | `/mic/v1/…` returns 404 |

## 3. Recorder, direct (on the service's host) and proxy (on the Pi)

Install from the release tarball for the host's architecture:

```bash
sudo install -m 0755 voicenotes-mic /usr/local/bin/
sudo useradd --system --groups audio,input voicenotes-mic
sudo install -m 0600 voicenotes-mic.env.example /usr/local/etc/voicenotes-mic.env   # then edit
sudo install -m 0644 voicenotes-mic.service /etc/systemd/system/ && sudo systemctl enable --now voicenotes-mic
```

Set `MIC_SERVER_URL` in `/usr/local/etc/voicenotes-mic.env`: `http://127.0.0.1:8080` for direct mode, or
`http://voicenotes.example.org:8080` on the Pi.

| Do | Expect |
|---|---|
| Tap, say a sentence with a task, tap | `🎙 recording…` appears, then the same message becomes `✓ saved · 1 task · open`. The note is `#inbox` with the WAV attached, timed at the first tap. The host's volume doesn't change. The spool is empty afterwards. |
| Tap, then tap again within 1 s | The start message disappears; no capture is created. |
| Tap, stay silent 5 s, tap | `🔇 no speech heard — not saved`; no note; `$DATA_DIR/nospeech/<id>.wav` exists on the service. |
| Set `MIC_MAX_LENGTH=30s`; tap and keep talking | The recording stops at 30 s and is saved; the next tap starts a new one. |
| Tap, then `systemctl kill -s KILL voicenotes-mic` after 20 s (it restarts) | A note with at least the first ~18 s. |
| Stop the service; make 3 recordings; start the service | No start messages for those 3; after the restart, 3 outcome messages and 3 notes, in recording order. |
| Unplug the receiver | Within about 1 min: `🎙 <name>: receiver not connected…`; `/healthz` degraded on both sides. Re-plug: `receiver connected`. |
| Power the Pi off | Within 6 min: `🎙 <name>: not heard from since HH:MM`. Power on: `back online`. |
| Fill the Pi's disk below `MIC_SPOOL_MIN_FREE` | A `storage full` alert; a tap does not start recording. Freeing space sends `storage OK`. |
| `curl -s 127.0.0.1:8081/healthz` on the recorder's host | The JSON status: receiver, storage, pending, rejected. |

## 4. Regression

- Every 001 and 002 acceptance test passes, except the 001 silence case, which now expects no note
  and `🔇 no speech heard — not saved` (FR-210a).
- The `voicenotes` image is still distroless and static: `CGO_ENABLED=0`, and the fence check in
  §0 is clean.
