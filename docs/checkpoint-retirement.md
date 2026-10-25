# Offline retirement of checkpoint history

This procedure discards protection against stale evidence for the exact deleted
keys. It is for intentionally and permanently retired repositories, never a way
to make an unhealthy backup healthy. Every real retirement needs explicit operator
approval of the keys, history loss, backup location and rollback plan. The project
has exercised it only on disposable synthetic checkpoints; no live retirement
or recovery is authorized or performed by these fixtures.

The 4,000-record cap counts active records and removed-inventory safety records.
An over-cap sample fails closed and leaves the last checkpoint intact. Do not
solve this by removing active records, raising the cap, deleting the whole file,
or discarding pending destination-change barriers. A process with failed writes
may hold newer invalidations in memory: restore persistence and require a
successful checkpoint before stopping. If that cannot be done, stop here and
resolve the lost-invalidation risk with the operator.

## Preparation and stop

1. Confirm the operator-approved retirement list against the complete configured
   inventory, retired infrastructure records and latest API sample. A key absent
   from today's inventory can still be temporarily removed and must then retain
   its barrier. Record the application revision, version, configuration and
   checkpoint checksum. Prevent configuration and inventory changes throughout.
2. Stop every writer through the deployment's approved maintenance process and
   prevent automatic restart. Do not delete its PVC or `.lock` file. Confirm no
   monitor process is using the volume. The tooling also checks the existing
   POSIX writer lock; an active writer is a hard failure.
3. On an offline copy of the volume, use the matching binary and configuration.
   Confirm available space for both backup and candidate. File/directory sync,
   POSIX locks and same-filesystem atomic rename must be supported. This runbook
   stages version **2** only. Preserve unsupported or corrupt files for diagnosis;
   do not reinterpret or downgrade them.

The commands below use operator-supplied absolute paths. Rehearse first in a
throwaway directory; they do not contain cluster, pod or service mutation commands.

```sh
backrestwatch --config "$CONFIG" --check-config
backrestwatch --config "$CONFIG" --check-checkpoint "$STATE"
python3 scripts/stage_checkpoint_retirement.py \
  --checkpoint "$STATE" --config "$CONFIG" \
  --backup "$BACKUP" --output "$CANDIDATE" \
  --retire retired-namespace/retired-cluster/repo1
backrestwatch --config "$CONFIG" --check-checkpoint "$BACKUP"
backrestwatch --config "$CONFIG" --check-checkpoint "$CANDIDATE"
```

`BACKUP` and `CANDIDATE` must be new, distinct files in the checkpoint directory.
The staging script takes the writer lock, copies the original bytes into a 0600
backup, removes only the named records in a separate 0600 candidate, and syncs
both files and the directory. It never replaces the checkpoint. It refuses active
inventory keys, unknown/duplicate keys, records still carrying saved successes,
non-version-2 input and duplicate JSON keys. The binary performs the application's
full version/JSON/entry validation before and after staging without an API call.
A failure may leave staging files; inspect and preserve them before retrying with
new filenames. Never overwrite a previous backup.

Review the JSON diff: exactly the approved entries must disappear. Every retained
`uid`, `created`, `repositoryID`, `notBefore`, `lastSuccess` and `source` must remain
unchanged. Check backup bytes against the recorded checksum. Keep a second backup
outside the writable checkpoint location according to the approved retention plan.
Ensure the candidate has the original application's required ownership and mode;
a root maintenance shell must not make the file unreadable to uid 25100.

## Atomic replacement and restart

While all writers remain stopped, replace by same-directory rename, never by
truncating/copying over the live path. The example syncs the containing directory:

```sh
python3 - "$CANDIDATE" "$STATE" <<'PY'
import os
import sys
from pathlib import Path
candidate, state = map(Path, sys.argv[1:])
if candidate.parent.resolve() != state.parent.resolve():
    raise SystemExit("same-directory candidate required")
os.replace(candidate, state)
fd = os.open(state.parent, os.O_RDONLY)
try:
    os.fsync(fd)
finally:
    os.close(fd)
PY
backrestwatch --config "$CONFIG" --check-checkpoint "$STATE"
```

Restart through the approved process. A valid checkpoint alone must leave
`/backups` unavailable until a **fresh complete API sample and successful
checkpoint write** confirm inventory, cluster identities and destinations. Use an
authorized monitoring client. Verify every active repository against the pre-edit
record; `/healthz` confirms only the process. Retained stale/change boundaries must
still reject old Job/CronJob and aggregate status. Do not erase barriers to clear
503. A retired key reintroduced later has lost its old protections and can accept
otherwise eligible old metadata on initial observation; explicitly review that
risk before reusing the key.

## Rollback

Stop writers again, preserving any newer checkpoint separately. Restoring the
pre-edit backup also rewinds any later successes or invalidations; an operator
must approve that consequence. Validate the backup with `--check-checkpoint`, copy
it to a new same-directory candidate, flush and sync that copy, restore original
ownership/mode, then use the same atomic replacement above. Keep the backup
unchanged. Restart and require a fresh API sample again. If identities or
destinations changed since the backup, normal evaluator boundaries apply; do not
force health or change real backup connections.

## Disposable validation

`make test` runs the actual Python staging helper against temporary JSON files.
`TestOfflineCheckpointRetirementAndRollback` exercises the store's exclusive lock,
backup, exact record deletion, version and JSON validation, same-directory atomic
replacement, monitor restart/fresh-sample gate, active success preservation,
stale aggregate rejection and rollback. All data and directories are synthetic.
Live PVC durability, permissions, scheduler stop/start, actual API/RBAC and
operator-selected retirement targets remain separate approval/acceptance work.
