#!/usr/bin/env python3
"""Builds docs/site/public/media/broker/walkthrough.mp4 from the footage and narration beside this file.

  raw/*.cast, raw/*.webm   footage, one file per section (docs/site/media/broker/walkthrough.record.ts)
  raw/sections.json        each section's wall-clock length and its marks: named on-screen events
                           in the file's own seconds
  narration/<id>.mp3       one file per narration part (narrate.py)
  edl.py                   the cut: each clip a source and a window between two marks (each plus an
                           offset), and the narration parts laid at marks inside it

Every cast is rendered through agg with idle time kept, so a cast's seconds are the video's
seconds. Every source is normalized to 1280x720 at 30 fps (the browser recordings are that size
already); each clip is cut hard to its window, with no speed change and no held frame; its audio
is silence the clip's length with its narration parts laid at their marks. The build fails when a
clip or part names a mark its section did not record, a window falls outside its file, a part
starts before its clip, runs past its clip's end or overlaps the next part, or a section's file
duration is off its wall-clock length (a browser recording outside its actions' and its page's
lengths by more than 5%, a cast more than 1.5 s shorter than its section or any longer). The clips
are concatenated into build/walkthrough.mp4, which then fails the build if it holds DEAD_AIR
seconds or more of silence over a frozen frame; only a video that passes is copied to OUT, the
video the site publishes, so a failed build leaves OUT as it was.

  python3 build.py            # rebuild (renders casts once, into build/) and publish to OUT
  python3 build.py --check    # verify the EDL against the footage and narration, write nothing
"""

from __future__ import annotations

import functools
import json
import re
import shutil
import subprocess
import sys
from pathlib import Path

from edl import CLIPS, At, Clip

HERE = Path(__file__).resolve().parent
RAW = HERE / "raw"
NARRATION = HERE / "narration"
BUILD = HERE / "build"
BUILT = BUILD / "walkthrough.mp4"
OUT = HERE.parents[2] / "public" / "media" / "broker" / "walkthrough.mp4"
W, H, FPS = 1280, 720, 30
BACKGROUND = "0x272822"  # agg's monokai background, so a terminal's padding is invisible
AGG = ["agg", "--font-size", "30", "--theme", "monokai", "--idle-time-limit", "3600", "--last-frame-duration", "0"]
CAPTURE_TOLERANCE = 0.05  # how far a browser recording may fall outside its wall-clock bounds
CAST_SLACK = 1.5  # how much shorter a cast may be than its section: the recorder's start and exit
DEAD_AIR = 2.0  # seconds of silence over a frozen frame the video may not hold
# Silence is below SILENCE_DB; a sound shorter than BLIP (a breath, a click) does not end a silence.
SILENCE_DB, BLIP = -45, 0.3


def run(*argv: str) -> str:
    return subprocess.run(argv, check=True, capture_output=True, text=True).stdout


@functools.cache
def duration(path: Path) -> float:
    return float(run("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", str(path)).strip())


@functools.cache
def cast_duration(path: Path) -> float:
    last = 0.0
    with path.open() as cast:
        next(cast)  # header
        for line in cast:
            last = json.loads(line)[0]
    return last


def source_duration(path: Path) -> float:
    """A footage file's length: a cast's last event time, a recording's container duration."""
    return cast_duration(path) if path.suffix == ".cast" else duration(path)


@functools.cache
def sections() -> dict[str, dict]:
    """raw/sections.json, which the recorder writes, keyed by file."""
    return {s["file"]: s for s in json.loads((RAW / "sections.json").read_text())}


def seconds(source: str, at: At) -> float:
    """Where `at` falls in `source`, in its own seconds. Besides the section's own marks, `start`
    is the file's first instant and `end` its last."""
    marks = {"start": 0.0, "end": source_duration(RAW / source), **sections()[source]["marks"]}
    if at.mark not in marks:
        raise KeyError(f"{source} recorded no mark {at.mark!r} (it has {', '.join(marks)})")
    return marks[at.mark] + at.offset


class Resolved:
    """A clip with its marks resolved: its window in source seconds, and each narration part's
    offset from the clip's start."""

    def __init__(self, clip: Clip):
        self.clip = clip
        self.start = seconds(clip.source, clip.start)
        self.end = seconds(clip.source, clip.end)
        self.narration = [(part, seconds(clip.source, at) - self.start) for part, at in clip.narration]

    @property
    def span(self) -> float:
        return self.end - self.start


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
    """Each section's (file, act seconds, wall seconds, file seconds). A screen capture under load
    can drop frames into a time-compressed file, shorter than the actions it shows; Playwright
    records a page from its first frame to its close, so an honest browser file lies between its
    actions' and its page's wall clocks. A cast is timing-accurate by construction and spans its
    section but the recorder's start and exit; it has no act seconds."""
    return [(file, s.get("actSeconds"), s["wallSeconds"], source_duration(RAW / file)) for file, s in sections().items()]


def check(clips: list[Clip]) -> tuple[list[Resolved], list[str]]:
    problems = []
    for file, act, wall, length in capture_rates():
        if act is None:
            if not wall - CAST_SLACK <= length <= wall:
                problems.append(f"{file}: {length:.2f}s of cast for a section that took {wall:.2f}s; re-record it")
        elif not act * (1 - CAPTURE_TOLERANCE) <= length <= wall * (1 + CAPTURE_TOLERANCE):
            problems.append(f"{file}: {length:.2f}s of footage for {act:.2f}s of actions in a page that lived {wall:.2f}s; re-record it")
    resolved = []
    for clip in clips:
        src = RAW / clip.source
        if not src.exists() or clip.source not in sections():
            problems.append(f"{clip.id}: no source {src} in raw/sections.json")
            continue
        try:
            r = Resolved(clip)
        except KeyError as missing:
            problems.append(f"{clip.id}: {missing.args[0]}")
            continue
        resolved.append(r)
        length = source_duration(src)
        if not 0 <= r.start < r.end <= length + 0.01:
            problems.append(f"{clip.id}: window {r.start:.2f}-{r.end:.2f}s is outside {clip.source} (0-{length:.2f}s)")
        cursor = 0.0
        for part, offset in r.narration:
            mp3 = NARRATION / f"{part}.mp3"
            if not mp3.exists():
                problems.append(f"{clip.id}: no narration {mp3.name} (run narrate.py)")
                continue
            spoken = duration(mp3)
            if offset < cursor:
                where = "before the clip starts" if cursor == 0 else f"into the part before it (ends {cursor:.2f}s)"
                problems.append(f"{clip.id}: {part} at {offset:.2f}s runs {where}")
            if offset + spoken > r.span:
                problems.append(f"{clip.id}: {part} runs {offset + spoken - r.span:.2f}s past the clip's end ({r.span:.2f}s); shorten the words or move it")
            cursor = offset + spoken
    return resolved, problems


def render(r: Resolved, index: int) -> Path:
    out = BUILD / f"clip-{index:02d}-{r.clip.id}.mp4"
    inputs = ["-ss", f"{r.start:.3f}", "-t", f"{r.span:.3f}", "-i", str(normalized(r.clip.source)),
              "-f", "lavfi", "-t", f"{r.span:.3f}", "-i", "anullsrc=r=44100:cl=stereo"]
    graph = []
    labels = ["[1:a]"]
    for n, (part, offset) in enumerate(r.narration):
        inputs += ["-i", str(NARRATION / f"{part}.mp3")]
        delay = int(offset * 1000)
        graph.append(f"[{n + 2}:a]aformat=sample_rates=44100:channel_layouts=stereo,volume=1.8,adelay={delay}|{delay}[n{n}]")
        labels.append(f"[n{n}]")
    graph.append(f"{''.join(labels)}amix=inputs={len(labels)}:duration=first:normalize=0[a]")
    run(
        "ffmpeg", "-y", "-v", "error", *inputs,
        "-filter_complex", ";".join(graph), "-map", "0:v", "-map", "[a]",
        "-c:v", "libx264", "-preset", "slow", "-crf", "16", "-tune", "stillimage", "-r", str(FPS), "-pix_fmt", "yuv420p",
        "-c:a", "aac", "-b:a", "192k", "-ar", "44100", "-ac", "2", "-t", f"{r.span:.3f}", str(out),
    )
    return out


def intervals(log: str, kind: str, total: float) -> list[tuple[float, float]]:
    """The (start, end) spans ffmpeg's silencedetect or freezedetect logged, `kind` being
    `silence` or `freeze`; a span still open at the end runs to `total`."""
    starts = [float(s) for s in re.findall(rf"{kind}_start: ([0-9.]+)", log)]
    ends = [float(e) for e in re.findall(rf"{kind}_end: ([0-9.]+)", log)]
    return list(zip(starts, ends + [total] * (len(starts) - len(ends))))


def dead_air(video: Path) -> list[tuple[float, float]]:
    """Every span of DEAD_AIR seconds or more where the video's audio is silent while its frame is
    frozen. Silences a blip shorter than BLIP apart count as one, so a breath cannot split a gap
    into two that each pass."""
    total = duration(video)
    log = subprocess.run(
        ["ffmpeg", "-v", "info", "-nostats", "-i", str(video),
         "-af", f"silencedetect=noise={SILENCE_DB}dB:d={BLIP}", "-vf", "freezedetect=n=0.001:d=0.5",
         "-f", "null", "-"],
        check=True, capture_output=True, text=True,
    ).stderr
    silences: list[tuple[float, float]] = []
    for start, end in intervals(log, "silence", total):
        if silences and start - silences[-1][1] < BLIP:
            silences[-1] = (silences[-1][0], end)
        else:
            silences.append((start, end))
    freezes = intervals(log, "freeze", total)
    found = []
    for s0, s1 in silences:
        for f0, f1 in freezes:
            start, end = max(s0, f0), min(s1, f1)
            if end - start >= DEAD_AIR:
                found.append((start, end))
    return found


def main() -> int:
    for tool in ("agg", "ffmpeg", "ffprobe"):
        if shutil.which(tool) is None:
            print(f"build.py: {tool} is required on PATH", file=sys.stderr)
            return 1
    resolved, problems = check(CLIPS)
    if problems:
        print("build.py: the EDL does not fit the footage and narration:", *problems, sep="\n  ", file=sys.stderr)
        return 1
    for file, act, wall, length in capture_rates():
        if act is None:
            print(f"build.py: {file}: {length:.2f}s of cast; section {wall:.2f}s (x{length / wall:.3f})")
        else:
            print(f"build.py: {file}: {length:.2f}s of footage; actions {act:.2f}s (x{length / act:.3f}), page life {wall:.2f}s (x{length / wall:.3f})")
    at = 0.0
    for r in resolved:
        parts = ", ".join(f"{part} at {at + offset:.1f}s" for part, offset in r.narration)
        print(f"build.py: {at:5.1f}s {r.clip.id}: {r.clip.source} {r.start:.2f}-{r.end:.2f}s ({r.span:.2f}s){'; ' + parts if parts else ''}")
        at += r.span
    if "--check" in sys.argv[1:]:
        print(f"build.py: {len(resolved)} clips, {at:.1f}s, all narration inside its clip")
        return 0
    rendered = [render(r, i) for i, r in enumerate(resolved)]
    listing = BUILD / "concat.txt"
    listing.write_text("".join(f"file '{path}'\n" for path in rendered))
    # The video is copied; the audio is decoded and encoded once more, so no clip's encoder
    # priming accumulates into drift across the joins.
    run("ffmpeg", "-y", "-v", "error", "-f", "concat", "-safe", "0", "-i", str(listing), "-c:v", "copy",
        "-c:a", "aac", "-b:a", "192k", "-ar", "44100", "-ac", "2", "-movflags", "+faststart", str(BUILT))
    print(f"build.py: wrote {BUILT} ({duration(BUILT):.1f}s)")
    dead = dead_air(BUILT)
    if dead:
        print(f"build.py: {BUILT.name} holds silence over a frozen frame for {DEAD_AIR:.0f}s or more at:",
              *(f"{start:.2f}-{end:.2f}s ({end - start:.2f}s)" for start, end in dead), sep="\n  ", file=sys.stderr)
        print(f"build.py: {OUT} is unchanged", file=sys.stderr)
        return 1
    print(f"build.py: no silence of {DEAD_AIR:.0f}s or more over a frozen frame")
    shutil.copyfile(BUILT, OUT)
    print(f"build.py: published {OUT}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
