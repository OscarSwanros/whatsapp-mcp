# wa-transcribe

On-device transcription for WhatsApp voice notes. macOS 26+, Swift 6, zero
third-party Swift dependencies.

Voice notes pile up and never get listened to. This turns one (or a whole
folder) into skimmable text, entirely on the Mac — audio never leaves the
machine. It reuses [Mira](../../atrialballoon/apps/mira)'s transcription engine
(Apple's `SpeechAnalyzer` + `SpeechTranscriber`), vendored here so the CLI stays
standalone. (HMB-327.)

## How it fits the WhatsApp flow

The WhatsApp bridge (`../whatsapp-bridge`) downloads a voice note's Ogg/Opus
file — `POST http://127.0.0.1:8080/api/download` with
`{ "message_id": "...", "chat_jid": "..." }`, which writes the `.ogg` and
returns its `path`. `wa-transcribe` takes that path, decodes it, and prints the
transcript:

```
bridge downloads .ogg  ──►  wa-transcribe <path>  ──►  transcript on stdout
     (network)                (ffmpeg + on-device Speech, all local)
```

## Pipeline

1. **Decode.** `AVAudioFile` / `SpeechAnalyzer` can't open Ogg/Opus, so the
   system ffmpeg (`/opt/homebrew/bin/ffmpeg`) decodes the input into a temporary
   16 kHz mono PCM WAV.
2. **Provision the model.** The requested locale is resolved against installed
   on-device models (`es_MX → es_ES → es`), downloading once if needed. This is
   the *only* network access.
3. **Transcribe on-device.** `SpeechAnalyzer` runs locally and streams
   timestamped segments, which are merged into sentence-level text.
4. **Emit.** The transcript prints to stdout; `--txt` also writes a sibling file.

## Build

```sh
cd ~/code/whatsapp-mcp/transcription
swift build -c release
```

The binary lands at `.build/release/wa-transcribe`.

## Usage

```sh
# One voice note (Mexican-Spanish default, handles English code-switching):
wa-transcribe ~/.config/homebase/whatsapp/<chat>/audio_20260708.ogg

# Force English:
wa-transcribe note.ogg --locale en_US

# A whole chat folder, writing sibling .txt files next to each note:
wa-transcribe ~/.config/homebase/whatsapp/<chat>/ --txt

# Clean transcript to a file (diagnostics stay on stderr):
wa-transcribe note.ogg > transcript.txt
```

Run `wa-transcribe --help` for all options (`--offline`, `--ffmpeg`,
`--list-locales`, `--keep-temp`, `--quiet`).

## Privacy

Transcription is on-device. Audio and transcripts are personal PII — this repo's
`.gitignore` excludes audio and `.txt` so neither is ever committed.
