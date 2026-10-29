#!/usr/bin/env python3
"""Stage an offline version-2 checkpoint edit; never replace the live checkpoint."""
import argparse
import fcntl
import json
import os
from pathlib import Path


def unique_object(pairs):
    result = {}
    seen = set()
    for key, value in pairs:
        folded = key.casefold()
        if folded in seen:
            raise ValueError("duplicate JSON key")
        seen.add(folded)
        result[key] = value
    return result


def stage(checkpoint, config, backup, output, retired):
    checkpoint, config, backup, output = map(Path, (checkpoint, config, backup, output))
    if not checkpoint.is_file() or checkpoint.is_symlink():
        raise ValueError("checkpoint must be an existing regular non-symlink file")
    if any(p.parent.resolve() != checkpoint.parent.resolve() for p in (backup, output)):
        raise ValueError("backup and candidate must be in the checkpoint directory")
    paths = [p.resolve() for p in (checkpoint, config, backup, output)]
    if len(set(paths)) != len(paths) or backup.exists() or output.exists():
        raise ValueError("use distinct paths and new backup/candidate filenames")
    if not retired or len(set(retired)) != len(retired):
        raise ValueError("supply an explicit unique retirement list")
    with open(str(checkpoint) + ".lock", "a", encoding="utf-8") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        raw = checkpoint.read_bytes()
        if len(raw) > 4 * 1024 * 1024:
            raise ValueError("checkpoint exceeds the application read budget")
        doc = json.loads(raw, object_pairs_hook=unique_object)
        inventory = json.loads(config.read_bytes(), object_pairs_hook=unique_object)
        if set(doc) != {"version", "entries"} or type(doc["version"]) is not int or doc["version"] != 2 or not isinstance(doc["entries"], dict):
            raise ValueError("maintenance requires an application-validated version-2 checkpoint")
        active = {f"{c['namespace']}/{c['name']}/{repo}" for c in inventory["clusters"] for repo in c["repos"]}
        for key in retired:
            if key in active or key not in doc["entries"]:
                raise ValueError("cannot retire an active or unknown repository")
            # Go's checkpoint decoder accepts case-insensitive field names.
            # Inspect the same fields without changing the retained JSON.
            entry = {name.casefold(): value for name, value in doc["entries"][key].items()}
            if entry.get("source") or entry.get("lastsuccess") not in (None, "0001-01-01T00:00:00Z"):
                raise ValueError("retirement is limited to records with no saved success")
        for key in retired:
            del doc["entries"][key]
        candidate = (json.dumps(doc, indent=2) + "\n").encode()
        for path, content in ((backup, raw), (output, candidate)):
            fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(fd, "wb") as file:
                file.write(content)
                file.flush()
                os.fsync(file.fileno())
        directory = os.open(checkpoint.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("checkpoint", "config", "backup", "output"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--retire", action="append", required=True)
    args = parser.parse_args()
    try:
        stage(args.checkpoint, args.config, args.backup, args.output, args.retire)
    except (OSError, ValueError, KeyError, TypeError) as error:
        parser.exit(1, f"Retirement was not staged: {error}\n")
    print("Backup and candidate staged. Original checkpoint unchanged. Validate before replacement.")


if __name__ == "__main__":
    main()
