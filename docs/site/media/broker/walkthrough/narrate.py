#!/usr/bin/env python3
"""Generates narration/<id>.mp3 for the narration parts edl.py names, through ElevenLabs.

  secrets ELEVENLABS_API_KEY -- uv run --no-project --with elevenlabs python3 narrate.py [ID...]

Regenerates the parts named, or every part when none is; each part is sent with the text of the
parts around it, so the voice carries from one to the next. The voice and its settings are the
constants below, and nowhere else.
"""

from __future__ import annotations

import os
import sys
from pathlib import Path

from elevenlabs import ElevenLabs, VoiceSettings, save

from edl import NARRATION

HERE = Path(__file__).resolve().parent
OUT = HERE / "narration"
VOICE_ID = "onwK4e9ZLuTAKqWW03F9"  # Daniel
MODEL_ID = "eleven_turbo_v2_5"
OUTPUT_FORMAT = "mp3_44100_192"
SETTINGS = VoiceSettings(stability=0.75, similarity_boost=0.85, style=0.0, speed=0.92, use_speaker_boost=True)


def main() -> int:
    key = os.environ.get("ELEVENLABS_API_KEY", "").strip()
    if key == "":
        print("narrate.py: ELEVENLABS_API_KEY is unset; run it as: secrets ELEVENLABS_API_KEY -- uv run --no-project --with elevenlabs python3 narrate.py", file=sys.stderr)
        return 1
    ids = list(NARRATION)
    wanted = sys.argv[1:] or ids
    unknown = [part for part in wanted if part not in NARRATION]
    if unknown:
        print(f"narrate.py: edl.py names no narration part {', '.join(unknown)}", file=sys.stderr)
        return 1
    client = ElevenLabs(api_key=key)
    OUT.mkdir(exist_ok=True)
    for part in wanted:
        i = ids.index(part)
        try:
            audio = client.text_to_speech.convert(
                voice_id=VOICE_ID,
                text=NARRATION[part],
                model_id=MODEL_ID,
                output_format=OUTPUT_FORMAT,
                voice_settings=SETTINGS,
                previous_text=" ".join(NARRATION[p] for p in ids[max(0, i - 2):i])[-300:] or None,
                next_text=" ".join(NARRATION[p] for p in ids[i + 1:i + 3])[:300] or None,
            )
            save(audio, str(OUT / f"{part}.mp3"))
        except Exception as error:  # the SDK's transport errors can carry request headers; print the type and status only
            print(f"narrate.py: {part}: {type(error).__name__} {getattr(error, 'status_code', '')}", file=sys.stderr)
            return 1
        print(f"narrate.py: wrote narration/{part}.mp3")
    return 0


if __name__ == "__main__":
    sys.exit(main())
