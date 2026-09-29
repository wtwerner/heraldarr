#!/usr/bin/env python3
"""Parity oracle: runs every testdata/scenarios/*.json through the reference Python implementation
(a single Python file, not vendored here; its path comes from $REFERENCE_PY) with
its network calls stubbed by the scenario data and a frozen clock, and writes what it produced to
testdata/expected/<scenario>.json:

  add_results   the log line add_import() returned for each event, in order
  pending       the pending batches after all events (what the batcher must hold)
  posts         one entry per Discord message, in send order:
                  card          the intermediate card dict
                  v2            the Components V2 body (first layout tried)
                  embed         classic embed + buttons (second layout)
                  embed_inline  embed with inline links (third layout)

The Go implementation must produce semantically equal JSON for each field. Deterministic: run with
any PYTHONHASHSEED (scenarios avoid ties that depend on set order)."""
import importlib.util
import json
import os
import sys
import tempfile
import types
import urllib.error
import urllib.parse
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
REF = Path(os.environ.get("REFERENCE_PY", "")).expanduser()
SCENARIOS = ROOT / "testdata" / "scenarios"
EXPECTED = ROOT / "testdata" / "expected"


def load_reference():
    if not REF.is_file():
        sys.exit("set REFERENCE_PY to the reference implementation (a .py file)")
    spec = importlib.util.spec_from_file_location("reference", REF)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def run(mod, sc, tmp):
    now = datetime.fromisoformat(sc["now"].replace("Z", "+00:00"))
    ts = now.timestamp()

    class FrozenDatetime(datetime):
        @classmethod
        def now(cls, tz=None):
            return now if tz else now.astimezone().replace(tzinfo=None)

    mod.time = types.SimpleNamespace(time=lambda: ts, sleep=lambda s: None)
    mod.datetime = FrozenDatetime
    for name in ("PENDING_FILE", "POSTED_FILE", "RT_CACHE_FILE", "HISTORY_FILE"):
        setattr(mod, name, tmp / getattr(mod, name).name)
    mod.save_json(mod.POSTED_FILE, {k: ts for k in sc["posted"]})

    def arr(source, path):
        if path not in sc["arr"]:
            raise urllib.error.URLError(f"no stub for {source} {path}")
        if sc["arr"][path] is None:
            raise urllib.error.HTTPError(f"http://arr/api/v3/{path}", 404, "Not Found", {}, None)
        return json.loads(json.dumps(sc["arr"][path]))
    mod.arr = arr

    def find(guids, arr_path, kind, fresh=False):
        return next((sc["plex"][g] for g in guids if g and g in sc["plex"]), None)
    mod.plex.server = sc["plex_server"]
    mod.plex.find = find
    mod.plex.season_key = lambda show, season: sc["plex_seasons"].get(f"{show['ratingKey']}:{season}")
    mod.plex.scan = lambda path: None

    def rotten_tomatoes(imdb_id, title):
        if imdb_id and imdb_id in sc["rt"]:
            return f"https://www.rottentomatoes.com/{sc['rt'][imdb_id]}"
        return "https://www.rottentomatoes.com/search?" + urllib.parse.urlencode({"search": title})
    mod.rotten_tomatoes = rotten_tomatoes

    posts = []

    def post_discord(webhook, card, src):
        posts.append({"source": next(k for k, v in mod.SOURCES.items() if v is src), "card": card,
                      "v2": mod.to_v2(card), "embed": mod.to_embed(card), "embed_inline": mod.to_embed(card, False)})
        return "v2"
    mod.post_discord = post_discord

    add_results = []
    for ev in sc["events"]:
        if ev["payload"].get("eventType") == "Download":
            add_results.append(mod.add_import(ev["source"], ev["payload"]))
    pending = mod.load_json(mod.PENDING_FILE, {})
    for key in sorted(pending):
        b = json.loads(json.dumps(pending[key]))
        b["plex_checks"] = mod.PLEX_MAX_CHECKS
        mod.send_batch(b, "https://discord.invalid/webhook", lambda keys: None, force=True, record=False)
    return {"add_results": add_results, "pending": pending, "posts": posts}


def main():
    mod = load_reference()
    EXPECTED.mkdir(parents=True, exist_ok=True)
    files = sorted(SCENARIOS.glob("*.json"))
    for f in files:
        sc = json.loads(f.read_text())
        with tempfile.TemporaryDirectory() as tmp:
            out = run(mod, sc, Path(tmp))
        (EXPECTED / f.name).write_text(json.dumps(out, indent=2, ensure_ascii=False, sort_keys=True) + "\n")
        print(f"{f.stem}: {len(out['pending'])} batch(es), {len(out['posts'])} post(s)")
    print(f"reference: {REF}")


if __name__ == "__main__":
    main()
