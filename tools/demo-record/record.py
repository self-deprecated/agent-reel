#!/usr/bin/env python3
"""Record the README demo: one agent explains AgentReel's web export, then
updates docs/exports.md, while a floating Zellij pane exports the very
recording that is running.

    python3 tools/demo-record/record.py --agent claude|codex|pi [--no-record]

It runs `areel attach` on a PTY of its own (a fixed-size "terminal"), so the
keys it types are real keystrokes of a real client. Each run uses a fresh
clone of this repository at HEAD in /tmp/agent-reel, a throwaway Zellij
session with a plain prompt and config, and a separate demo library, so no
personal paths, sessions or recordings end up on screen. Replies are
detected by watching the screen settle (`zellij action dump-screen`), never
by fixed sleeps. Recordings stay in the demo library; export them with
`areel export --library ...` afterwards.
"""

import argparse
import fcntl
import json
import os
import pty
import random
import shutil
import struct
import subprocess
import sys
import termios
import threading
import time

PROMPTS = [
    "How does the web export work in areel? A few sentences, and point me at the code.",
    'Nice. Add a short "Publish on GitHub Pages" example to docs/exports.md, 15 lines max.',
]
POPOVER = [
    "git diff --stat",
    "areel export @ demo.html --format web --single-file",
]

# What is typed to launch each agent; the demo shell's functions add the flags.
AGENTS = {"claude": "claude", "codex": "codex", "pi": "pi"}
TRUST = ["trust the files", "trust this folder", "trust the contents", "do you trust"]
PERMISSION = ["Do you want to proceed?", "Allow command?", "Would you like to run", "Approve?"]


def log(msg):
    print(f"[demo {time.strftime('%H:%M:%S')}] {msg}", flush=True)


class Demo:
    def __init__(self, args):
        self.a = args
        self.agent = AGENTS[args.agent]
        self.work = args.work
        self.lib = os.path.join(self.work, "lib")
        self.session = f"demo-{args.agent}"
        self.rec = None
        self.screens = os.path.join(self.work, "screens", args.agent)
        self.step = 0
        env = {k: v for k, v in os.environ.items() if not k.startswith(("ZELLIJ", "CLAUDE", "T3CODE"))}
        env.update(
            AREEL_LIBRARY=self.lib,
            AREEL_BACKEND="zellij",
            ZELLIJ_CONFIG_FILE=os.path.join(self.work, "zellij.kdl"),
            TERM="xterm-256color",
            COLORTERM="truecolor",
        )
        env.pop("AREEL_RECORDING", None)
        self.env = env

    # --- setup ---------------------------------------------------------------

    def prepare(self):
        os.makedirs(self.screens, exist_ok=True)
        os.makedirs(self.lib, mode=0o700, exist_ok=True)
        os.chmod(self.lib, 0o700)
        repo = subprocess.run(["git", "-C", os.path.dirname(os.path.abspath(__file__)), "rev-parse", "--show-toplevel"],
                              capture_output=True, text=True, check=True).stdout.strip()
        co = self.a.checkout
        if os.path.exists(co):
            shutil.rmtree(co)
        subprocess.run(["git", "clone", "--quiet", "--no-hardlinks", repo, co], check=True)
        subprocess.run(["git", "-C", co, "checkout", "--quiet", "--detach",
                        subprocess.run(["git", "-C", repo, "rev-parse", "HEAD"], capture_output=True, text=True, check=True).stdout.strip()], check=True)
        subprocess.run(["git", "-C", co, "remote", "remove", "origin"], check=True)
        rc = os.path.join(self.work, "bashrc")
        with open(rc, "w") as f:
            f.write(f"""HISTFILE=/dev/null
export GIT_PAGER=cat PAGER=cat
PS1='\\[\\e[1;35m\\]agent-reel\\[\\e[0m\\] \\$ '
claude() {{ command claude --plugin-dir {co}/agent-plugin --permission-mode auto --disallowedTools Agent Task "$@"; }}
codex() {{ command codex -s workspace-write -a never "$@"; }}
pi() {{ PI_DISABLE_NATIVE_TEXT_CLIPBOARD=1 command pi "$@"; }}
cd {co}
""")
        shell = os.path.join(self.work, "shell")
        with open(shell, "w") as f:
            f.write(f"#!/bin/sh\nexec bash --noprofile --rcfile {rc} -i\n")
        os.chmod(shell, 0o755)
        with open(self.env["ZELLIJ_CONFIG_FILE"], "w") as f:
            f.write(f"""default_shell "{shell}"
default_layout "compact"
pane_frames false
show_startup_tips false
show_release_notes false
session_serialization false
""")

    def zellij(self, *args, check=True):
        return subprocess.run(["zellij", *args], env=self.env, capture_output=True, text=True, check=check)

    def areel(self, *args):
        # From the checkout, so the library groups the recording under it.
        out = subprocess.run(["areel", *args, "--library", self.lib], env=self.env, capture_output=True, text=True,
                             cwd=self.a.checkout)
        if out.returncode != 0:
            raise SystemExit(f"areel {' '.join(args)}: {out.stderr.strip()}")
        return out.stdout

    def start_session(self):
        self.zellij("delete-session", "--force", self.session, check=False)
        self.zellij("attach", "--create-background", self.session)
        pid, fd = pty.fork()
        if pid == 0:
            fcntl.ioctl(0, termios.TIOCSWINSZ, struct.pack("HHHH", self.a.rows, self.a.cols, 0, 0))
            os.execvpe("areel", ["areel", "attach", self.session], self.env)
        self.pid, self.fd = pid, fd
        fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", self.a.rows, self.a.cols, 0, 0))
        threading.Thread(target=self.drain, daemon=True).start()
        self.wait_for(lambda s: "agent-reel" in s, "the shell prompt", timeout=30)

    def drain(self):
        while True:
            try:
                if not os.read(self.fd, 65536):
                    return
            except OSError:
                return

    # --- screen --------------------------------------------------------------

    def screen(self):
        return self.zellij("--session", self.session, "action", "dump-screen", check=False).stdout

    def save(self, name):
        self.step += 1
        with open(os.path.join(self.screens, f"{self.step:02d}-{name}.txt"), "w") as f:
            f.write(self.screen())

    def at_shell(self):
        lines = [l for l in self.screen().splitlines() if l.strip()]
        return bool(lines) and lines[-1].strip() == "agent-reel $"

    def wait_for(self, pred, what, timeout=60):
        end = time.time() + timeout
        while time.time() < end:
            if pred(self.screen()):
                return
            time.sleep(0.4)
        self.save("timeout")
        raise SystemExit(f"timed out waiting for {what}; see {self.screens}")

    def settle(self, what, quiet, timeout, min_time=0.0):
        """Wait until the screen has changed and then stayed still for quiet s."""
        start = time.time()
        first = last = self.screen()
        changed_at, changed = time.time(), False
        while time.time() - start < timeout:
            time.sleep(0.5)
            s = self.screen()
            if s != last:
                last, changed_at = s, time.time()
                changed = changed or s != first
            if changed and time.time() - changed_at >= quiet and time.time() - start >= min_time:
                return s
        self.save("timeout")
        raise SystemExit(f"timed out waiting for {what}; see {self.screens}")

    # --- typing --------------------------------------------------------------

    def type(self, text, enter=True):
        for ch in text:
            os.write(self.fd, ch.encode())
            delay = random.uniform(0.035, 0.11)
            if ch in ",.?":
                delay += random.uniform(0.15, 0.3)
            elif ch == " ":
                delay += random.uniform(0.0, 0.05)
            time.sleep(delay)
        if enter:
            time.sleep(random.uniform(0.4, 0.7))
            os.write(self.fd, b"\r")

    def answer_trust(self):
        """Move the dialog's cursor to its "yes" choice, then confirm."""
        lines = self.screen().splitlines()
        cur = next(i for i, l in enumerate(lines) if l.lstrip().startswith(("❯", "›")))
        near = range(max(0, cur - 3), min(len(lines), cur + 4))
        yes = next(i for i in near if "yes" in lines[i].lower() or "trust and" in lines[i].lower())
        for _ in range(abs(yes - cur)):
            os.write(self.fd, b"\x1b[B" if yes > cur else b"\x1b[A")
            time.sleep(0.4)
        time.sleep(0.6)
        os.write(self.fd, b"\r")

    def mark(self, label):
        if self.rec:
            self.areel("mark", label, self.rec)
            log(f"mark {label!r}")

    # --- the script ----------------------------------------------------------

    def run(self):
        self.prepare()
        self.start_session()
        log(f"session {self.session} attached at {self.a.cols}x{self.a.rows}")
        if not self.a.no_record:
            self.rec = json.loads(self.areel("record", self.session, "--json"))["id"]
            self.areel("keys", "on", self.rec)
            log(f"recording {self.rec}")
        time.sleep(1.5)

        self.mark(f"Start {self.a.agent}")
        self.type(self.agent)
        self.settle("the agent to start", quiet=3, timeout=90, min_time=2)
        if any(t in self.screen().lower() for t in TRUST):
            self.save("trust")
            time.sleep(1.0)
            self.answer_trust()
            self.settle("the agent after the trust prompt", quiet=3, timeout=60)
        self.save("ready")
        if self.at_shell():
            raise SystemExit(f"{self.a.agent} exited at start; see {self.screens}")

        for label, prompt in zip(["Ask about web export", "Update the docs"], PROMPTS):
            time.sleep(1.0)
            self.mark(label)
            self.type(prompt)
            self.settle(f"the reply to {label!r}", quiet=self.a.quiet, timeout=600, min_time=4)
            for _ in range(5):
                if not any(p in self.screen() for p in PERMISSION):
                    break
                self.save("permission")
                log("approving a permission prompt")
                os.write(self.fd, b"\r")
                self.settle(f"the reply to {label!r}", quiet=self.a.quiet, timeout=600)
            self.save(label.split()[0].lower())
            time.sleep(2.0)

        self.mark("Export this recording")
        self.zellij("--session", self.session, "action", "new-pane", "--floating", "--name", "export",
                    "--x", "18%", "--y", "12%", "--width", "64%", "--height", "60%")
        self.wait_for(lambda s: "agent-reel" in s and "$" in s, "the floating shell", timeout=20)
        time.sleep(0.8)
        for cmd in POPOVER:
            self.type(cmd)
            time.sleep(0.5)
            self.wait_for(lambda _: self.at_shell(), f"`{cmd}`", timeout=120)
            time.sleep(1.5)
        self.save("popover")
        time.sleep(1.5)
        self.type("exit")
        time.sleep(4.0)
        self.save("done")
        if self.rec:
            self.areel("stop", self.rec)
            self.wait_sealed()
        self.close()

    def wait_sealed(self):
        end = time.time() + 120
        while time.time() < end:
            if os.path.exists(os.path.join(self.lib, f"{self.rec}.areel")):
                log(f"sealed {self.lib}/{self.rec}.areel")
                return
            time.sleep(1)
        raise SystemExit("recording did not seal; run areel status")

    def close(self):
        self.zellij("kill-session", self.session, check=False)
        self.zellij("delete-session", "--force", self.session, check=False)
        try:
            os.waitpid(self.pid, 0)
        except ChildProcessError:
            pass


def main():
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("--agent", choices=AGENTS, required=True)
    p.add_argument("--work", default="/tmp/areel-rec", help="scratch directory; the demo library is WORK/lib")
    p.add_argument("--checkout", default="/tmp/agent-reel", help="fresh clone the agent works in")
    p.add_argument("--cols", type=int, default=110)
    p.add_argument("--rows", type=int, default=32)
    p.add_argument("--quiet", type=float, default=8.0, help="seconds of a still screen that end a reply")
    p.add_argument("--no-record", action="store_true", help="dry run: drive the session without recording")
    args = p.parse_args()
    demo = Demo(args)
    try:
        demo.run()
    except BaseException:
        demo.save("failed") if hasattr(demo, "fd") else None
        if hasattr(demo, "fd"):
            demo.close()
        raise


if __name__ == "__main__":
    main()
