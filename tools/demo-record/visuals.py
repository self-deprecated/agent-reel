#!/usr/bin/env python3
"""Build the README visuals from the demo recordings made by record.py:

  docs/demo/<agent>.html     one web export per agent (the demo page's tabs)
  docs/images/hero.gif       Claude Code's run, composed and sped up
  docs/images/compare.png    every agent's docs edit side by side

    nix develop -c python3 tools/demo-record/visuals.py

Run it inside `nix develop`: composing needs Chromium and FFmpeg.
"""

import argparse
import json
import os
import subprocess
import tempfile

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
TITLES = {"claude": "Claude Code", "codex": "Codex", "pi": "Pi"}
REDACT = [{"preset": "secrets"}, {"preset": "emails"}, {"preset": "homepaths"}]


def areel(lib, *args):
    return subprocess.run(["areel", *args, "--library", lib], check=True, capture_output=True, text=True).stdout


def compose(lib, edl, *args):
    with tempfile.NamedTemporaryFile("w", suffix=".edl.json", delete=False) as f:
        json.dump(edl, f)
    try:
        areel(lib, "compose", f.name, *args)
    finally:
        os.remove(f.name)


def main():
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("--library", default="/tmp/areel-rec/lib")
    a = p.parse_args()
    for tool in ("CHROMIUM", "FFMPEG", "FFPROBE"):
        os.environ.setdefault(f"AREEL_EXPORT_{tool}", os.environ.get(f"AREEL_TEST_{tool}", ""))
    recs = {r["target"]["name"].removeprefix("demo-"): r["id"]
            for r in json.loads(areel(a.library, "ls", "--json"))["items"]
            if r["target"]["name"].startswith("demo-")}
    agents = [x for x in TITLES if x in recs]
    os.makedirs(os.path.join(ROOT, "docs", "demo"), exist_ok=True)
    os.makedirs(os.path.join(ROOT, "docs", "images"), exist_ok=True)

    for agent in agents:
        out = os.path.join(ROOT, "docs", "demo", f"{agent}.html")
        if os.path.exists(out):
            os.remove(out)
        marks = json.loads(areel(a.library, "marks", recs[agent], "--json"))["marks"]
        first_answer = next(m["seconds"] for m in marks if m.get("label") == "Update the docs") - 1
        areel(a.library, "export", recs[agent], out, "--format", "web", "--single-file", "--idle-limit", "2",
              "--poster", f"npt:{first_answer:.1f}",
              "--redact", "secrets,emails,homepaths",
              "--title", f"{TITLES[agent]} explains AgentReel's web export, then documents it")
        print("wrote", out)

    chapters = [("Ask about web export", "Update the docs", 3, "1. Ask how the web export works"),
                ("Update the docs", "Export this recording", 3, "2. Ask for a docs example"),
                ("Export this recording", None, 2, "3. Export this very recording")]
    hero = {
        "schema": "agent-reel/edl/1",
        "output": {"width": 1280, "height": 720, "rate": {"num": 12, "den": 1}},
        "defaults": {"idle": {"threshold": "1.5s", "keep": "0.5s"}},
        "sources": {"main": {"recording": recs["claude"]}},
        "redact": REDACT,
        "segments": [seg for i, (start, end, speed, text) in enumerate(chapters) for seg in (
            {"id": f"card{i}", "kind": "card", "duration": "1.5s", "title": text},
            dict({"id": f"clip{i}", "source": "main", "from": {"mark": start}, "speed": {"num": speed, "den": 1}},
                 **({"to": {"mark": end}} if end else {})))],
    }
    with tempfile.TemporaryDirectory() as tmp:
        mp4 = os.path.join(tmp, "hero.mp4")
        compose(a.library, hero, mp4)
        gif = os.path.join(ROOT, "docs", "images", "hero.gif")
        # A small diff-rectangle palette GIF: terminal frames change little.
        subprocess.run([os.environ["AREEL_EXPORT_FFMPEG"], "-loglevel", "error", "-y", "-i", mp4, "-vf",
                        "fps=8,scale=960:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=64:stats_mode=diff[p];"
                        "[b][p]paletteuse=dither=none:diff_mode=rectangle", "-loop", "0", gif], check=True)
        print("wrote", gif)

    if len(agents) > 1:
        compare = {
            "schema": "agent-reel/edl/1",
            # Tall enough for side-by-side 110x32 terminals, plus the gutter.
            "output": {"width": 1920, "height": min(1080, int(1920 / len(agents) / 1.62) // 2 * 2 + 40)},
            "defaults": {"minFontPx": 6},
            "sources": {x: {"recording": recs[x]} for x in agents},
            "redact": REDACT,
            "segments": [{"id": "compare", "kind": "layout", "layout": "side-by-side", "clock": agents[0],
                          "from": {"mark": "Export this recording", "offset": "-3s"}, "duration": "1",
                          "slots": [{"source": x, "from": {"mark": "Export this recording", "offset": "-3s"}}
                                    for x in agents]}],
        }
        png = os.path.join(ROOT, "docs", "images", "compare.png")
        if os.path.exists(png):
            os.remove(png)
        compose(a.library, compare, "--still", "compare@0", png)
        os.remove(png + ".metadata.json")  # recording IDs and tool versions: not for the repo
        print("wrote", png)


if __name__ == "__main__":
    main()
