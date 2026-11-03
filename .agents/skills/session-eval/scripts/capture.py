#!/usr/bin/env python3
"""Capture Polly SQLite evidence without resuming or modifying a session."""

import argparse
from collections import Counter, defaultdict
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import sqlite3
import tempfile


def utc_now():
    return datetime.now(timezone.utc).isoformat()


def ns_time(value):
    return datetime.fromtimestamp(value / 1e9, timezone.utc).isoformat()


def read_connection(path):
    connection = sqlite3.connect(path.resolve().as_uri() + "?mode=ro", uri=True)
    connection.row_factory = sqlite3.Row
    connection.execute("PRAGMA query_only=ON")
    return connection


def check_schema(db):
    required = {
        "sessions": {"id", "name", "parent_id", "created_ns", "updated_ns", "settings_json"},
        "messages": {"session_id", "sequence", "payload_json"},
    }
    optional = {
        "swarm_members": {"parent_id", "member_id"},
        "swarm_records": {"parent_id", "kind", "id", "payload_json"},
        "session_leases": {"session_id", "heartbeat_ns", "expires_ns"},
    }
    tables = {row[0] for row in db.execute("SELECT name FROM sqlite_master WHERE type='table'")}
    for table, needed in {**required, **optional}.items():
        if table not in tables and table in optional:
            continue
        columns = {row[1] for row in db.execute(f'PRAGMA table_info("{table}")')}
        if not needed <= columns:
            raise ValueError(f"Unsupported schema: {table} missing {sorted(needed - columns)}")
    return tables


def write_json(path, value):
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def session_info(row):
    settings = json.loads(row["settings_json"])
    return {
        "id": row["id"].hex(),
        "name": row["name"],
        "title": settings.get("title"),
        "parent_id": row["parent_id"].hex() if row["parent_id"] else None,
        "created_utc": ns_time(row["created_ns"]),
        "updated_utc": ns_time(row["updated_ns"]),
    }


def export_capture(db, output, latest, names):
    tables = check_schema(db)
    rows = list(db.execute("SELECT * FROM sessions ORDER BY updated_ns DESC, created_ns DESC, id"))
    by_id = {row["id"]: row for row in rows}
    children = defaultdict(set)
    member_ids = set()
    for row in rows:
        if row["parent_id"]:
            children[row["parent_id"]].add(row["id"])
    if "swarm_members" in tables:
        for row in db.execute("SELECT parent_id, member_id FROM swarm_members"):
            children[row["parent_id"]].add(row["member_id"])
            member_ids.add(row["member_id"])
    roots = [row for row in rows if row["parent_id"] is None and row["id"] not in member_ids]
    selected = []
    if names:
        for name in names:
            matches = [row for row in rows if row["name"] == name or row["id"].hex() == name.lower()]
            if len(matches) != 1:
                raise ValueError(f"Session selector must match exactly one name or full ID: {name!r}")
            if matches[0]["id"] not in selected:
                selected.append(matches[0]["id"])
    else:
        selected = [row["id"] for row in roots[:latest]]
    if not selected:
        raise ValueError("No matching root sessions in this store")

    families = []
    all_ids = set()
    warnings = []
    for root in selected:
        family, pending = set(), [root]
        while pending:
            current = pending.pop()
            if current in family:
                continue
            if current not in by_id:
                warnings.append(f"Missing referenced member: {current.hex()}")
                continue
            family.add(current)
            pending.extend(children[current] - family)
        families.append({"selected_id": root.hex(), "session_ids": sorted(s.hex() for s in family)})
        all_ids.update(family)
        if by_id[root]["parent_id"] or root in member_ids:
            warnings.append(f"Selected worker {root.hex()}; select its root for parent coordination")

    sessions = []
    for sid in sorted(all_ids):
        row = by_id[sid]
        folder = output / "sessions" / sid.hex()
        folder.mkdir(parents=True)
        info = session_info(row)
        counts = Counter()
        first, last = None, None
        with (folder / "messages.jsonl").open("w", encoding="utf-8") as target:
            for message in db.execute("SELECT sequence,payload_json FROM messages WHERE session_id=? ORDER BY sequence", (sid,)):
                payload = json.loads(message["payload_json"])
                sequence = message["sequence"]
                first = sequence if first is None else first
                last = sequence
                counts[payload.get("role", "unknown")] += 1
                target.write(json.dumps({"sequence": sequence, "message": payload}, ensure_ascii=False) + "\n")
        info.update(message_count=sum(counts.values()), role_counts=dict(counts), first_sequence=first, last_sequence=last)
        write_json(folder / "session.json", {**info, "settings": json.loads(row["settings_json"])})
        sessions.append(info)

    records = Counter()
    with (output / "coordination.jsonl").open("w", encoding="utf-8") as target:
        if "swarm_records" in tables:
            for sid in sorted(all_ids):
                for row in db.execute("SELECT kind,id,payload_json FROM swarm_records WHERE parent_id=? ORDER BY kind,id", (sid,)):
                    records[row["kind"]] += 1
                    target.write(json.dumps({"parent_id": sid.hex(), "kind": row["kind"], "id": row["id"], "payload": json.loads(row["payload_json"])}, ensure_ascii=False) + "\n")
        else:
            warnings.append("No swarm_records table; coordination evidence unavailable")
    leases = []
    if "session_leases" in tables:
        for sid in sorted(all_ids):
            for row in db.execute("SELECT heartbeat_ns,expires_ns FROM session_leases WHERE session_id=?", (sid,)):
                leases.append({"session_id": sid.hex(), "heartbeat_utc": ns_time(row["heartbeat_ns"]), "expires_utc": ns_time(row["expires_ns"])})
    return {
        "selection_basis": "explicit name/full ID" if names else "root last activity, descending",
        "selected_ids": [sid.hex() for sid in selected],
        "recent_roots": [session_info(row) for row in roots[:10]],
        "families": families, "sessions": sessions,
        "coordination_counts": dict(records), "leases": leases,
        "warnings": warnings,
    }


def capture(source, output=None, latest=1, names=None):
    source = Path(source).expanduser().resolve(strict=True)
    if latest < 1:
        raise ValueError("--latest must be positive")
    if output:
        output = Path(output).expanduser().absolute()
        output.mkdir(mode=0o700)  # Refuse to overwrite an existing capture.
    else:
        output = Path(tempfile.mkdtemp(prefix="polly-session-eval-"))
    started = utc_now()
    snapshot = output / "sessions.db"
    source_db = read_connection(source)
    try:
        target_db = sqlite3.connect(snapshot)
        try:
            source_db.backup(target_db)
        finally:
            target_db.close()
    finally:
        source_db.close()
    snapshot.chmod(0o600)
    captured = utc_now()
    db = read_connection(snapshot)
    try:
        integrity = [row[0] for row in db.execute("PRAGMA quick_check")]
        if integrity != ["ok"]:
            raise ValueError(f"Snapshot integrity check failed: {integrity}")
        manifest = export_capture(db, output, latest, names)
    finally:
        db.close()
    manifest.update(source=str(source), snapshot=str(snapshot), capture_started_utc=started, captured_utc=captured, quick_check="ok")
    write_json(output / "manifest.json", manifest)
    return output, manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--db", type=Path, default=Path.home() / ".pollytool" / "polly.db")
    parser.add_argument("--out", type=Path, help="New output directory; defaults to private temporary storage")
    group = parser.add_mutually_exclusive_group()
    group.add_argument("--latest", type=int, default=1, help="Number of roots by last activity (default 1)")
    group.add_argument("--session", action="append", help="Exact name or full hex ID; repeat to select multiple")
    args = parser.parse_args()
    previous_umask = os.umask(0o077)
    try:
        output, manifest = capture(args.db, args.out, args.latest, args.session)
    except (OSError, sqlite3.Error, ValueError) as error:
        parser.exit(1, f"Capture failed: {error}\n")
    finally:
        os.umask(previous_umask)
    print(json.dumps({"capture": str(output), "selected": [s for s in manifest["sessions"] if s["id"] in manifest["selected_ids"]], "family_sessions": len(manifest["sessions"]), "warnings": manifest["warnings"]}, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
