import fcntl
import importlib.util
import json
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parents[2] / "scripts/stage_checkpoint_retirement.py"
spec = importlib.util.spec_from_file_location("retirement", SCRIPT)
retirement = importlib.util.module_from_spec(spec)
spec.loader.exec_module(retirement)


class RetirementTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.checkpoint = self.root / "evidence.json"
        self.config = self.root / "config.json"
        self.backup = self.root / "before.json"
        self.output = self.root / "candidate.json"
        self.doc = {"version": 2, "entries": {
            "db/db/repo1": {"uid": "active", "lastSuccess": "2026-10-25T01:00:00Z", "source": "PostgresCluster scheduled-backup status"},
            "db/db/repo2": {"uid": "active", "notBefore": "2026-10-25T02:00:00Z", "lastSuccess": "0001-01-01T00:00:00Z", "source": ""},
            "old/old/repo1": {"uid": "retired", "lastSuccess": "0001-01-01T00:00:00Z", "source": ""},
        }}
        self.checkpoint.write_text(json.dumps(self.doc))
        self.config.write_text(json.dumps({"clusters": [{"namespace": "db", "name": "db", "repos": {"repo1": {}, "repo2": {}}}]}))

    def stage(self, keys):
        retirement.stage(self.checkpoint, self.config, self.backup, self.output, keys)

    def test_stages_only_explicit_retirement_and_preserves_original(self):
        original = self.checkpoint.read_bytes()
        self.stage(["old/old/repo1"])
        candidate = json.loads(self.output.read_bytes())
        self.assertEqual(candidate["entries"], {k: v for k, v in self.doc["entries"].items() if k != "old/old/repo1"})
        self.assertEqual(self.checkpoint.read_bytes(), original)
        self.assertEqual(self.backup.read_bytes(), original)
        self.assertEqual(self.output.stat().st_mode & 0o777, 0o600)

    def test_rejects_active_unknown_duplicate_and_nonretired_records(self):
        for keys in (["db/db/repo1"], ["db/db/repo2"], ["missing/missing/repo1"], [], ["old/old/repo1"] * 2):
            with self.assertRaises(ValueError):
                self.stage(keys)
        self.doc["entries"]["old/old/repo1"]["source"] = "old success"
        self.checkpoint.write_text(json.dumps(self.doc))
        with self.assertRaises(ValueError):
            self.stage(["old/old/repo1"])
        self.assertFalse(self.output.exists())

    def test_active_writer_and_existing_outputs_are_never_overwritten(self):
        with open(str(self.checkpoint) + ".lock", "a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaises(BlockingIOError):
                self.stage(["old/old/repo1"])
        self.backup.write_bytes(b"preserve")
        with self.assertRaises(ValueError):
            self.stage(["old/old/repo1"])
        self.assertEqual(self.backup.read_bytes(), b"preserve")

    def test_version_and_duplicate_json_validation(self):
        for text in ('{"version":99,"entries":{}}', '{"version":2,"Version":2,"entries":{}}', '{"version":2,"entries":{"x":{},"x":{}}}'):
            self.checkpoint.write_text(text)
            with self.assertRaises(ValueError):
                self.stage(["old/old/repo1"])


if __name__ == "__main__":
    unittest.main()
