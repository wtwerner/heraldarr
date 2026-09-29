#!/usr/bin/env python3
"""Writes testdata/scenarios/*.json: synthetic *arr webhook events plus the API/Plex/RT data a run
needs, all fictional (titles, IDs, URLs). Scenarios are the parity spec for the Go port; edit them
here and re-run, never by hand. Then run `make harness` to regenerate testdata/expected/."""
import json
from datetime import datetime, timedelta, timezone
from pathlib import Path

OUT = Path(__file__).resolve().parent.parent / "scenarios"
NOW = datetime(2026, 6, 15, 12, 0, tzinfo=timezone.utc)
IMG = "https://images.example.org/{}/{}.jpg"
PLEX_SERVER = {"id": "0123456789abcdef0123456789abcdef01234567", "name": "Example Server"}


def iso(dt):
    return dt.strftime("%Y-%m-%dT%H:%M:%SZ")


def days(n):
    return NOW - timedelta(days=n)


def images(slug):
    return [{"coverType": c, "url": f"/MediaCover/{slug}/{c}.jpg", "remoteUrl": IMG.format(c, slug)}
            for c in ("poster", "fanart", "banner")]


# --- Sonarr ---------------------------------------------------------------------------------------

def series(sid, title, year, genres, slug):
    return {"id": sid, "title": title, "titleSlug": slug, "path": f"/data/media/TV/{title} ({year})",
            "tvdbId": 900000 + sid, "tvMazeId": 0, "tmdbId": 0, "imdbId": f"tt99{sid:05d}", "type": "standard",
            "year": year, "genres": genres, "images": images(slug), "tags": []}


def episode(eid, sid, season, number, title, aired):
    return {"id": eid, "episodeNumber": number, "seasonNumber": season, "title": title,
            "overview": "An episode.", "airDate": aired.strftime("%Y-%m-%d"), "airDateUtc": iso(aired),
            "seriesId": sid, "tvdbId": 5000000 + eid}


def sonarr_import(s, eps, quality="WEBDL-1080p", upgrade=False, deleted=False):
    e0 = eps[0]
    rel = f"Season {e0['seasonNumber']:02d}/{s['title']} - S{e0['seasonNumber']:02d}E{e0['episodeNumber']:02d}.mkv"
    body = {"eventType": "Download", "instanceName": "Sonarr", "applicationUrl": "", "series": s, "episodes": eps,
            "episodeFile": {"id": 70000 + e0["id"], "relativePath": rel, "path": f"{s['path']}/{rel}",
                            "quality": quality, "qualityVersion": 1, "releaseGroup": "EXAMPLE",
                            "sceneName": "Example.Release", "size": 1500000000, "dateAdded": iso(NOW)},
            "isUpgrade": upgrade, "downloadClient": "qBittorrent", "downloadClientType": "qBittorrent",
            "downloadId": f"HASH{e0['id']}"}
    if deleted:
        body["deletedFiles"] = [{"id": 1, "relativePath": "old.mkv", "path": "/old.mkv", "quality": "HDTV-720p"}]
    return {"source": "sonarr", "payload": body}


def series_detail(s, network, status, seasons, overview, rating=None, cert=None, next_airing=None, last_aired=None):
    """seasons: {number: (files, total)}"""
    d = {"id": s["id"], "title": s["title"], "year": s["year"], "network": network, "status": status,
         "overview": overview, "certification": cert, "ratings": {"votes": 100, "value": rating or 0},
         "statistics": {"episodeFileCount": sum(f for f, _ in seasons.values())},
         "seasons": [{"seasonNumber": n, "monitored": True,
                      "statistics": {"episodeFileCount": f, "totalEpisodeCount": t, "episodeCount": t}}
                     for n, (f, t) in sorted(seasons.items())]}
    if next_airing:
        d["nextAiring"] = iso(next_airing)
    if last_aired:
        d["lastAired"] = iso(last_aired)
    return d


def plex_show(key, rating=None, cert=None):
    m = {"ratingKey": key, "type": "show"}
    if rating is not None:
        m.update(audienceRating=rating, audienceRatingImage="themoviedb://image.rating")
    if cert:
        m["contentRating"] = cert
    return m


# --- Radarr ---------------------------------------------------------------------------------------

def movie(mid, title, year, genres, slug, overview, uhd=False):
    root = "Movies 4K" if uhd else "Movies"
    return {"id": mid, "title": title, "year": year, "releaseDate": f"{year}-03-01",
            "folderPath": f"/data/media/{root}/{title} ({year})", "tmdbId": 800000 + mid,
            "imdbId": f"tt98{mid:05d}", "overview": overview, "genres": genres, "images": images(slug), "tags": []}


def radarr_import(source, m, quality="Bluray-1080p", size=9_800_000_000, media=None, upgrade=False):
    media = media or {"audioChannels": 5.1, "audioCodec": "EAC3", "audioLanguages": ["eng"], "height": 1080,
                      "width": 1920, "subtitles": ["eng"], "videoCodec": "x264", "videoDynamicRange": "",
                      "videoDynamicRangeType": ""}
    rel = f"{m['title']} ({m['year']}).mkv"
    return {"source": source, "payload": {
        "eventType": "Download", "instanceName": "Radarr", "applicationUrl": "", "movie": m,
        "remoteMovie": {"tmdbId": m["tmdbId"], "imdbId": m["imdbId"], "title": m["title"], "year": m["year"]},
        "movieFile": {"id": 60000 + m["id"], "relativePath": rel, "path": f"{m['folderPath']}/{rel}",
                      "quality": quality, "qualityVersion": 1, "releaseGroup": "EXAMPLE", "sceneName": "Example.Release",
                      "indexerFlags": "", "size": size, "dateAdded": iso(NOW), "mediaInfo": media},
        "isUpgrade": upgrade, "downloadClient": "qBittorrent", "downloadClientType": "qBittorrent",
        "downloadId": f"HASHM{m['id']}"}}


def movie_detail(m, imdb=None, rt=None, cert=None, runtime=0, collection=None, trailer=None):
    ratings = {}
    if imdb is not None:
        ratings["imdb"] = {"votes": 1000, "value": imdb, "type": "user"}
    if rt is not None:
        ratings["rottenTomatoes"] = {"votes": 0, "value": rt, "type": "user"}
    d = {"id": m["id"], "title": m["title"], "year": m["year"], "overview": m["overview"], "ratings": ratings,
         "certification": cert, "runtime": runtime, "youTubeTrailerId": trailer, "hasFile": True}
    if collection:
        d["collection"] = {"title": collection, "tmdbId": 1}
    return d


def credits(director, cast):
    out = [{"personName": director, "type": "crew", "job": "Director", "department": "Directing"}]
    out += [{"personName": n, "type": "cast", "character": "Someone", "order": i} for i, n in enumerate(cast)]
    out.append({"personName": "A. Producer", "type": "crew", "job": "Producer", "department": "Production"})
    return out


def plex_movie(key, rating=None):
    m = {"ratingKey": key, "type": "movie"}
    if rating is not None:
        m.update(audienceRating=rating, audienceRatingImage="imdb://image.rating")
    return m


def guid_tv(s):
    return f"tvdb://{s['tvdbId']}"


def guid_movie(m):
    return f"tmdb://{m['tmdbId']}"


SCENARIOS = {}


def scenario(name, description, events, arr=None, plex=None, seasons=None, rt=None, posted=None):
    SCENARIOS[name] = {"description": description, "now": iso(NOW), "plex_server": PLEX_SERVER,
                       "posted": posted or [], "events": events, "arr": arr or {}, "plex": plex or {},
                       "plex_seasons": seasons or {}, "rt": rt or {}}


# --- TV scenarios ---------------------------------------------------------------------------------

s = series(101, "Harbor Lights", 2026, ["Drama", "Mystery", "Crime", "Thriller"], "harbor-lights")
scenario("tv_new_series_pilot", "Brand-new series: the pilot aired 2 days ago, only file Sonarr has.",
         [sonarr_import(s, [episode(1001, 101, 1, 1, "Low Tide", days(2))])],
         arr={"series/101": series_detail(s, "Example Network", "continuing", {1: (1, 10)},
                                          "A harbor town keeps its secrets until a lighthouse keeper goes missing.",
                                          rating=7.9, cert="TV-14", next_airing=NOW + timedelta(days=5))},
         plex={guid_tv(s): plex_show("5101", rating=8.1, cert="TV-14")}, seasons={"5101:1": "5102"},
         rt={s["imdbId"]: "tv/harbor_lights"})

s = series(102, "Glass Orchard", 2022, ["Comedy"], "glass-orchard")
scenario("tv_weekly_episode", "Continuing series, one new weekly episode aired yesterday; next episode in 6 days.",
         [sonarr_import(s, [episode(2035, 102, 3, 5, "The Graft", days(1))])],
         arr={"series/102": series_detail(s, "Streamy", "continuing", {1: (10, 10), 2: (10, 10), 3: (5, 10)},
                                          "Siblings run a failing orchard.", rating=8.0, cert="TV-MA",
                                          next_airing=NOW + timedelta(days=6))},
         plex={guid_tv(s): plex_show("5201", rating=8.4, cert="TV-MA")}, seasons={"5201:3": "5203"})

s = series(103, "Paper Comets", 2021, ["Science Fiction", "Adventure"], "paper-comets")
eps = [episode(3100 + n, 103, 2, n, f"Chapter {n}", days(400) + timedelta(days=7 * n)) for n in range(1, 11)]
scenario("tv_season_pack", "A whole back-catalog season (10 files, one Download event each); season 1 already on disk.",
         [sonarr_import(s, [e], quality="Bluray-1080p") for e in eps],
         arr={"series/103": series_detail(s, "Orbit", "ended", {1: (10, 10), 2: (10, 10)},
                                          "Kids build a rocket out of newspaper.", rating=7.2, cert="TV-PG",
                                          last_aired=days(300))},
         plex={guid_tv(s): plex_show("5301", rating=7.5, cert="TV-PG")}, seasons={"5301:2": "5303"},
         rt={s["imdbId"]: "tv/paper_comets"})

s = series(104, "Northbound", 2015, ["Drama", "Western"], "northbound")
eps = [episode(4000 + se * 100 + n, 104, se, n, f"Mile {se}-{n}", days(3000) + timedelta(days=7 * (se * 10 + n)))
       for se in (1, 2, 3) for n in range(1, 9)]
scenario("tv_backlog_complete_series", "An ended series added from scratch: seasons 1-3, all 24 episodes.",
         [sonarr_import(s, [e]) for e in eps],
         arr={"series/104": series_detail(s, "Frontier", "ended", {0: (0, 2), 1: (8, 8), 2: (8, 8), 3: (8, 8)},
                                          "A cattle drive across three winters.", rating=8.6, cert="TV-14",
                                          last_aired=days(2500))},
         plex={guid_tv(s): plex_show("5401", rating=8.8, cert="TV-14")})

s = series(105, "Salt & Signal", 2019, ["Documentary"], "salt-and-signal")
eps = [episode(5040 + n, 105, 4, n, f"Part {n}", days(200) + timedelta(days=7 * n)) for n in (1, 2, 3, 7, 8)]
scenario("tv_partial_season", "Five non-contiguous episodes of a season that already had 3 files: an episodes card with ranges.",
         [sonarr_import(s, [e], quality="HDTV-720p") for e in eps],
         arr={"series/105": series_detail(s, "Public Example", "continuing", {1: (6, 6), 2: (6, 6), 3: (6, 6), 4: (8, 10)},
                                          "Coastal radio stations.", rating=7.0, cert="TV-G")},
         plex={guid_tv(s): plex_show("5501")}, seasons={"5501:4": "5504"})

s = series(106, "The Quiet Engine", 2024, ["Animation"], "the-quiet-engine")
scenario("tv_series_deleted", "Series deleted from Sonarr before the batch went out (API 404): post from webhook data.",
         [sonarr_import(s, [episode(6001, 106, 1, 3, "Idle", days(3))])],
         arr={"series/106": None}, plex={})

s = series(107, "Glass Orchard Specials", 2023, ["Comedy"], "glass-orchard-specials")
scenario("tv_special", "A single special (season 0) of a continuing series, aired 20 days ago.",
         [sonarr_import(s, [episode(7001, 107, 0, 2, "Holiday Harvest", days(20))])],
         arr={"series/107": series_detail(s, "Streamy", "continuing", {0: (2, 3), 1: (8, 8)}, "Specials.")},
         plex={guid_tv(s): plex_show("5701")})

s = series(102, "Glass Orchard", 2022, ["Comedy"], "glass-orchard")
scenario("tv_upgrade_ignored", "An upgrade (isUpgrade) and a replaced file (deletedFiles): nothing is queued.",
         [sonarr_import(s, [episode(2034, 102, 3, 4, "Pruning", days(8))], upgrade=True),
          sonarr_import(s, [episode(2033, 102, 3, 3, "Frost", days(15))], deleted=True)])

scenario("tv_already_announced", "Re-import of an episode announced within 30 days: queued nothing, second one goes out.",
         [sonarr_import(s, [episode(2035, 102, 3, 5, "The Graft", days(1))]),
          sonarr_import(s, [episode(2036, 102, 3, 6, "Grafted", days(0.5))])],
         arr={"series/102": series_detail(s, "Streamy", "continuing", {1: (10, 10), 2: (10, 10), 3: (6, 10)},
                                          "Siblings run a failing orchard.", rating=8.0, cert="TV-MA",
                                          next_airing=NOW + timedelta(days=6))},
         plex={guid_tv(s): plex_show("5201", rating=8.4, cert="TV-MA")}, seasons={"5201:3": "5203"},
         posted=["sonarr:ep:2035"])

s = series(108, "Lamplight Relay", 2018, ["Drama"], "lamplight-relay")
eps = [episode(8000 + se * 100 + n, 108, se, n, f"Relay {se}-{n}", days(2000) + timedelta(days=7 * (se * 10 + n)))
       for se in (1, 2) for n in range(1, 6)]
scenario("tv_whole_seasons_stale_stats",
         "Two whole seasons arrive, but Sonarr's stats still count 4 of season 2's 5 files: the season 2 line "
         "isn't 'complete', yet the whole-seasons line still replaces both.",
         [sonarr_import(s, [e]) for e in eps],
         arr={"series/108": series_detail(s, "Example Network", "ended", {1: (5, 5), 2: (4, 5)},
                                          "Night-shift signal operators.", last_aired=days(1500))},
         plex={guid_tv(s): plex_show("5801")})

# --- Movie scenarios ------------------------------------------------------------------------------

M = [
    movie(201, "The Long Meridian", 2025, ["Science Fiction", "Thriller", "Drama", "Mystery"], "the-long-meridian",
          "A navigator discovers the map is lying. " * 12),
    movie(202, "Lantern Season", 2024, ["Romance"], "lantern-season", "Two rivals share a festival stall."),
    movie(203, "The Brass Choir", 2023, ["Music", "Drama"], "the-brass-choir", "A mining town's band goes on strike."),
    movie(204, "Undertow", 2025, ["Horror"], "undertow", "Something pulls swimmers out past the buoys."),
    movie(205, "A Map of Small Hours", 2022, ["Animation", "Family"], "a-map-of-small-hours", "A clock runs backwards."),
]
DETAIL = {
    201: movie_detail(M[0], imdb=7.4, rt=91, cert="PG-13", runtime=124, collection="The Meridian Collection",
                      trailer="EXAMPLE0001"),
    202: movie_detail(M[1], imdb=6.1, cert="PG", runtime=98),
    203: movie_detail(M[2], rt=100, runtime=59),
    204: movie_detail(M[3], imdb=5.8, rt=42, cert="R", runtime=91, trailer="EXAMPLE0004"),
    205: movie_detail(M[4], imdb=8.2, rt=97, cert="G", runtime=87, collection="Small Hours"),
}
CREDITS = {
    201: credits("Rae Okafor", ["Lena Marsh", "Tomas Vey", "Ida Brandt", "Olu Park"]),
    202: credits("Sam Oduya", ["Kit Lorne"]),
    203: [],
    204: credits("Jon Harrow", ["Mae Lind", "Ezra Quill", "Ana Sol"]),
    205: credits("Pia Novak", ["Voice One", "Voice Two"]),
}


def movie_arr(ids):
    out = {}
    for i in ids:
        out[f"movie/{i}"] = DETAIL[i]
        out[f"credit?movieId={i}"] = CREDITS[i]
    return out


scenario("movie_single", "One movie: scores, certification, runtime, credits, collection, trailer, fanart.",
         [radarr_import("radarr", M[0])], arr=movie_arr([201]),
         plex={guid_movie(M[0]): plex_movie("6201")}, rt={M[0]["imdbId"]: "m/the_long_meridian"})

scenario("movie_three", "Three movies in one quiet window: one card each.",
         [radarr_import("radarr", m, quality="WEBDL-1080p") for m in M[1:4]], arr=movie_arr([202, 203, 204]),
         plex={guid_movie(M[1]): plex_movie("6202"), guid_movie(M[3]): plex_movie("6204")})

scenario("movie_digest", "Five movies in one quiet window: one digest card with an image grid.",
         [radarr_import("radarr", m) for m in M], arr=movie_arr([201, 202, 203, 204, 205]),
         plex={guid_movie(m): plex_movie(f"6{m['id']}") for m in M if m["id"] != 203})

UHD = movie(301, "The Long Meridian", 2025, ["Science Fiction", "Thriller"], "the-long-meridian",
            "A navigator discovers the map is lying.", uhd=True)
HDR = {"audioChannels": 7.1, "audioCodec": "TrueHD Atmos", "audioLanguages": ["eng"], "height": 2160, "width": 3840,
       "subtitles": ["eng"], "videoCodec": "x265", "videoDynamicRange": "HDR", "videoDynamicRangeType": "DV HDR10Plus"}
scenario("movie_4k_private", "A 4K movie on the private route: 'New in 4K', purple, technical footer.",
         [radarr_import("radarr4k", UHD, quality="Remux-2160p", size=58_300_000_000, media=HDR)],
         arr={"movie/301": movie_detail(UHD, imdb=7.4, rt=91, cert="PG-13", runtime=124, trailer="EXAMPLE0001"),
              "credit?movieId=301": CREDITS[201]},
         plex={guid_movie(UHD): plex_movie("7301")}, rt={UHD["imdbId"]: "m/the_long_meridian"})

UHDS = [movie(310 + i, t, 2020 + i, ["Drama"], f"uhd-{i}", "An overview.", uhd=True)
        for i, t in enumerate(["Cinder", "Bellwether", "Anchor Point", "Driftwood"])]
scenario("movie_4k_digest", "Four 4K movies at once on the private route: '4 new 4K movies' digest.",
         [radarr_import("radarr4k", m, quality="WEBDL-2160p", media=HDR) for m in UHDS],
         arr={f"movie/{m['id']}": movie_detail(m, imdb=6.5 + i / 10, runtime=100 + i) for i, m in enumerate(UHDS)},
         plex={guid_movie(m): plex_movie(f"73{m['id']}") for m in UHDS})

scenario("movie_not_in_plex", "Plex never found it and Radarr lost the movie: no Plex button, IMDb link as heading.",
         [radarr_import("radarr", M[4])], arr={}, plex={})

scenario("movie_upgrade_ignored", "A Radarr upgrade is never announced.",
         [radarr_import("radarr", M[1], upgrade=True)])

if __name__ == "__main__":
    OUT.mkdir(parents=True, exist_ok=True)
    for old in OUT.glob("*.json"):
        old.unlink()
    for name, sc in SCENARIOS.items():
        (OUT / f"{name}.json").write_text(json.dumps(sc, indent=2, ensure_ascii=False) + "\n")
    print(f"wrote {len(SCENARIOS)} scenarios to {OUT}")
