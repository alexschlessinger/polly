"""Behavioral checks using a synthetic, live WAL store; no user sessions."""

import json
from pathlib import Path
import sqlite3
import tempfile
import unittest

from capture import capture, read_connection


class CaptureTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="session-eval-test-")
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.source = self.base / "source.db"
        self.db = sqlite3.connect(self.source)
        self.addCleanup(self.db.close)
        self.db.execute("PRAGMA journal_mode=WAL")
        self.db.execute("PRAGMA wal_autocheckpoint=0")
        self.db.executescript("""
            CREATE TABLE sessions(id BLOB PRIMARY KEY, name TEXT, parent_id BLOB,
                created_ns INTEGER, updated_ns INTEGER, settings_json TEXT);
            CREATE TABLE messages(session_id BLOB, sequence INTEGER, payload_json TEXT);
            CREATE TABLE swarm_members(parent_id BLOB, member_id BLOB);
            CREATE TABLE swarm_records(parent_id BLOB, kind TEXT, id TEXT, payload_json TEXT);
            CREATE TABLE session_leases(session_id BLOB, heartbeat_ns INTEGER, expires_ns INTEGER);
        """)
        self.root = bytes.fromhex("01" * 16)
        self.other = bytes.fromhex("02" * 16)
        self.child = bytes.fromhex("03" * 16)
        self.grandchild = bytes.fromhex("04" * 16)
        self.legacy_child = bytes.fromhex("05" * 16)
        for sid, name, parent, created, updated in [
            (self.root, "resumed-root", None, 10, 100),
            (self.other, "newer-created-root", None, 20, 80),
            (self.child, "worker", self.root, 30, 200),
            (self.grandchild, "nested-worker", self.child, 40, 210),
            (self.legacy_child, "legacy-member", None, 50, 220),
        ]:
            self.db.execute("INSERT INTO sessions VALUES(?,?,?,?,?,?)", (sid, name, parent, created, updated, '{"title":"fixture"}'))
        self.db.execute("INSERT INTO swarm_members VALUES(?,?)", (self.root, self.legacy_child))
        self.message = {"role": "assistant", "text_blocks": [{"phase": "commentary", "text": "exact text"}], "metadata": {"turn_audit": {"status": "completed"}}}
        self.db.execute("INSERT INTO messages VALUES(?,?,?)", (self.root, 7, json.dumps(self.message)))
        self.db.execute("INSERT INTO swarm_records VALUES(?,?,?,?)", (self.root, "future_record_kind", "x", '{"kept":true}'))
        self.db.execute("INSERT INTO session_leases VALUES(?,?,?)", (self.root, 100, 300))
        self.db.commit()

    def test_live_wal_root_selection_family_and_raw_evidence(self):
        before = self.db.total_changes
        output, manifest = capture(self.source, self.base / "capture")
        self.assertEqual(manifest["selected_ids"], [self.root.hex()])
        self.assertEqual(set(manifest["families"][0]["session_ids"]), {x.hex() for x in (self.root, self.child, self.grandchild, self.legacy_child)})
        self.assertEqual(manifest["coordination_counts"], {"future_record_kind": 1})
        messages = (output / "sessions" / self.root.hex() / "messages.jsonl").read_text()
        self.assertEqual(json.loads(messages), {"sequence": 7, "message": self.message})
        self.assertEqual(manifest["leases"][0]["session_id"], self.root.hex())
        self.assertEqual(self.db.total_changes, before)
        self.assertEqual(output.stat().st_mode & 0o777, 0o700)
        snapshot = read_connection(output / "sessions.db")
        self.addCleanup(snapshot.close)
        self.assertEqual(snapshot.execute("SELECT count(*) FROM messages").fetchone()[0], 1)
        with self.assertRaises(sqlite3.OperationalError):
            snapshot.execute("DELETE FROM messages")

    def test_multiple_roots_and_explicit_name_or_id(self):
        _, manifest = capture(self.source, self.base / "two", latest=2)
        self.assertEqual(manifest["selected_ids"], [self.root.hex(), self.other.hex()])
        _, manifest = capture(self.source, self.base / "named", names=["newer-created-root", self.other.hex()])
        self.assertEqual(manifest["selected_ids"], [self.other.hex()])
        _, manifest = capture(self.source, self.base / "worker", names=["legacy-member"])
        self.assertIn("Selected worker", manifest["warnings"][0])

    def test_refuses_overwrite_bad_selector_and_unsupported_schema(self):
        existing = self.base / "existing"
        existing.mkdir()
        (existing / "sentinel").write_text("keep")
        with self.assertRaises(FileExistsError):
            capture(self.source, existing)
        self.assertEqual((existing / "sentinel").read_text(), "keep")
        with self.assertRaisesRegex(ValueError, "match exactly one"):
            capture(self.source, self.base / "missing", names=["absent"])
        self.db.execute("DROP TABLE messages")
        self.db.commit()
        with self.assertRaisesRegex(ValueError, "Unsupported schema"):
            capture(self.source, self.base / "invalid")


if __name__ == "__main__":
    unittest.main()
