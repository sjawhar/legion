#!/usr/bin/env python3
"""Builds docs/site/public/media/broker/walkthrough.mp4 from the footage and narration beside this file.

  raw/*.cast, raw/*.webm   footage, one file per section (docs/site/media/broker/walkthrough.record.ts)
  narration/<id>.mp3       one file per narration part (narrate.py)
  edl.py                   the cut: each clip a source, a window in source seconds, and the
                           narration parts laid at offsets inside it

Every cast is rendered through agg with idle time kept, so a cast's seconds are the video's
seconds. Every source is normalized to 1280x720 at 30 fps (the browser recordings are that size
already); each clip is cut hard to its window, with no speed change and no held frame; its audio
is silence the clip's length with its narration parts laid at their offsets, and the build fails
when a part runs past the end of its clip or overlaps the next part, or when a section's file
duration is off its wall-clock length (raw/sections.json): a browser recording outside its
actions' and its page's lengths by more than 5%, a cast more than 1.5 s shorter than its section
or any longer. The clips are concatenated into OUT, the video the site publishes.

  python3 build.py            # rebuild (renders casts once, into build/)
  python3 build.py --check    # verify the EDL against the footage and narration, write nothing
"""

from __future__ import annotations

import json
import shutil
import subprocess
import sys
from pathlib import Path

from edl import CLIPS, Clip

HERE = Path(__file__).resolve().parent
RAW = HERE / "raw"
NARRATION = HERE / "narration"
BUILD = HERE / "build"
OUT = HERE.parents[2] / "public" / "media" / "broker" / "walkthrough.mp4"
W, H, FPS = 1280, 720, 30
BACKGROUND = "0x272822"  # agg's monokai background, so a terminal's padding is invisible
AGG = ["agg", "--font-size", "30", "--theme", "monokai", "--idle-time-limit", "3600", "--last-frame-duration", "0"]
CAPTURE_TOLERANCE = 0.05  # how far a browser recording may fall outside its wall-clock bounds
CAST_SLACK = 1.5  # how much shorter a cast may be than its section: the recorder's start and exit


def run(*argv: str) -> str:
    return subprocess.run(argv, check=True, capture_output=True, text=True).stdout


def duration(path: Path) -> float:
    return float(run("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", str(path)).strip())


def cast_duration(path: Path) -> float:
    last = 0.0
    with path.open() as cast:
        next(cast)  # header
        for line in cast:
            last = json.loads(line)[0]
    return last


def normalized(source: str) -> Path:
    """The source as a 1280x720, 30 fps, silent H.264 file, rendered once into build/."""
    src = RAW / source
    out = BUILD / f"{Path(source).stem}.norm.mp4"
    if out.exists() and out.stat().st_mtime >= src.stat().st_mtime:
        return out
    BUILD.mkdir(exist_ok=True)
    video = src
    if src.suffix == ".cast":
        gif = BUILD / f"{src.stem}.gif"
        run(*AGG, str(src), str(gif))
        video = gif
    run(
        "ffmpeg", "-y", "-v", "error", "-i", str(video),
        "-vf", f"scale={W}:{H}:force_original_aspect_ratio=decrease:flags=lanczos,pad={W}:{H}:(ow-iw)/2:(oh-ih)/2:color={BACKGROUND},fps={FPS},format=yuv420p",
        "-an", "-c:v", "libx264", "-preset", "slow", "-crf", "16", "-tune", "stillimage", str(out),
    )
    return out


def capture_rates() -> list[tuple[str, float | None, float, float]]:
    """Each section's (file, act seconds, wall seconds, file seconds), from raw/sections.json,
    which the recorder writes. A screen capture under load can drop frames into a time-compressed
    file, shorter than the actions it shows; Playwright records a page from its first frame to its
    close, so an honest browser file lies between its actions' and its page's wall clocks. A cast
    is timing-accurate by construction and spans its section but the recorder's start and exit;
    it has no act seconds."""
    sections = json.loads((RAW / "sections.json").read_text())
    return [(s["file"], s.get("actSeconds"), s["wallSeconds"],
             cast_duration(RAW / s["file"]) if s["file"].endswith(".cast") else duration(RAW / s["file"]))
            for s in sections]


def check(clips: list[Clip]) -> list[str]:
    problems = []
    for file, act, wall, length in capture_rates():
        if act is None:
            if not wall - CAST_SLACK <= length <= wall:
                problems.append(f"{file}: {length:.2f}s of cast for a section that took {wall:.2f}s; re-record it")
        elif not act * (1 - CAPTURE_TOLERANCE) <= length <= wall * (1 + CAPTURE_TOLERANCE):
            problems.append(f"{file}: {length:.2f}s of footage for {act:.2f}s of actions in a page that lived {wall:.2f}s; re-record it")
    for clip in clips:
        src = RAW / clip.source
        if not src.exists():
            problems.append(f"{clip.id}: no source {src}")
            continue
        length = cast_duration(src) if src.suffix == ".cast" else duration(src)
        if not 0 <= clip.start < clip.end <= length + 0.01:
            problems.append(f"{clip.id}: window {clip.start}-{clip.end}s is outside {clip.source} (0-{length:.2f}s)")
        span = clip.end - clip.start
        cursor = 0.0
        for part, offset in clip.narration:
            mp3 = NARRATION / f"{part}.mp3"
            if not mp3.exists():
                problems.append(f"{clip.id}: no narration {mp3.name} (run narrate.py)")
                continue
            spoken = duration(mp3)
            if offset < cursor:
                problems.append(f"{clip.id}: {part} at {offset}s overlaps the part before it (ends {cursor:.2f}s)")
            if offset + spoken > span:
                problems.append(f"{clip.id}: {part} runs {offset + spoken - span:.2f}s past the clip's end ({span:.2f}s); shorten the words or move it")
            cursor = offset + spoken
    return problems


def render(clip: Clip, index: int) -> Path:
    span = clip.end - clip.start
    out = BUILD / f"clip-{index:02d}-{clip.id}.mp4"
    inputs = ["-ss", f"{clip.start:.3f}", "-t", f"{span:.3f}", "-i", str(normalized(clip.source)),
              "-f", "lavfi", "-t", f"{span:.3f}", "-i", "anullsrc=r=44100:cl=stereo"]
    graph = []
    labels = ["[1:a]"]
    for n, (part, offset) in enumerate(clip.narration):
        inputs += ["-i", str(NARRATION / f"{part}.mp3")]
        delay = int(offset * 1000)
        graph.append(f"[{n + 2}:a]aformat=sample_rates=44100:channel_layouts=stereo,volume=1.8,adelay={delay}|{delay}[n{n}]")
        labels.append(f"[n{n}]")
    graph.append(f"{''.join(labels)}amix=inputs={len(labels)}:duration=first:normalize=0[a]")
    run(
        "ffmpeg", "-y", "-v", "error", *inputs,
        "-filter_complex", ";".join(graph), "-map", "0:v", "-map", "[a]",
        "-c:v", "libx264", "-preset", "slow", "-crf", "16", "-tune", "stillimage", "-r", str(FPS), "-pix_fmt", "yuv420p",
        "-c:a", "aac", "-b:a", "192k", "-ar", "44100", "-ac", "2", "-t", f"{span:.3f}", str(out),
    )
    return out


def main() -> int:
    for tool in ("agg", "ffmpeg", "ffprobe"):
        if shutil.which(tool) is None:
            print(f"build.py: {tool} is required on PATH", file=sys.stderr)
            return 1
    problems = check(CLIPS)
    if problems:
        print("build.py: the EDL does not fit the footage and narration:", *problems, sep="\n  ", file=sys.stderr)
        return 1
    for file, act, wall, length in capture_rates():
        if act is None:
            print(f"build.py: {file}: {length:.2f}s of cast; section {wall:.2f}s (x{length / wall:.3f})")
        else:
            print(f"build.py: {file}: {length:.2f}s of footage; actions {act:.2f}s (x{length / act:.3f}), page life {wall:.2f}s (x{length / wall:.3f})")
    if "--check" in sys.argv[1:]:
        print(f"build.py: {len(CLIPS)} clips, {sum(c.end - c.start for c in CLIPS):.1f}s, all narration inside its clip")
        return 0
    rendered = [render(clip, i) for i, clip in enumerate(CLIPS)]
    listing = BUILD / "concat.txt"
    listing.write_text("".join(f"file '{path}'\n" for path in rendered))
    # The video is copied; the audio is decoded and encoded once more, so no clip's encoder
    # priming accumulates into drift across the joins.
    run("ffmpeg", "-y", "-v", "error", "-f", "concat", "-safe", "0", "-i", str(listing), "-c:v", "copy",
        "-c:a", "aac", "-b:a", "192k", "-ar", "44100", "-ac", "2", "-movflags", "+faststart", str(OUT))
    print(f"build.py: wrote {OUT} ({duration(OUT):.1f}s)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
