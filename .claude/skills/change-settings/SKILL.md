---
name: change-settings
description: Use when adding, renaming or changing a user setting, a default, a prompt preset, or anything that crosses the Wails bridge as a Go struct the settings blob stores.
---

# Changing settings

The settings blob is **one JSON row** decoded over a fully populated default. That
is the whole migration story, and it is why a field can be added without a schema
change — and why two rules are absolute.

## Steps

1. **Field** in `internal/config/config.go` (`Settings`), with a short comment for
   anything whose meaning is not obvious from the name.

2. **Default** in `Default()`. A zero value is rarely the right default: an absent
   field keeps the default, so the default is what every existing install gets.

3. **Normalise** in `normalise()` if the value can arrive out of range or empty
   from an older blob, a hand-edited JSON, or the UI.

4. **Clone** in `Clone()` if the field holds a map, a slice or a pointer. This is
   not optional: `TestCloneSharesNothingWithTheOriginal` fails when a reference
   field is missed, because a later `json.Marshal` would walk a map another
   goroutine is writing.

5. **Secret?** Then it does not go in the blob. Keys live in `internal/secret` and
   are folded back in by `config.Load` (see the Brave key, each backend's key, and
   the image server's credential).

6. **UI.** Add the control in `Settings.tsx`, and label it in
   `frontend/src/lib/i18n.ts` in all three languages (`de`, `en`, `zh`). A
   hardcoded label is a bug, not a shortcut.

7. **Bindings.** A changed Go struct that crosses the bridge means `wails build`
   (or `wails generate module`) before `tsc` — `frontend/wailsjs/go/models.ts` is
   generated. Skip it and typecheck fails on the field you just added.

8. **Tests.** `internal/config/config_test.go` for the default and the decode;
   root-level `settings_save_test.go` / `settings_snapshot_test.go` when the save
   path or concurrency is involved. A settings field that only the UI knows about
   is a field with no test.

## Rules

- **Never change an existing field's type.** Old blobs are decoded into it; a type
  change is a silent data loss for every user who upgrades.
- **No migration code** is needed for a new field — `Default()` is the migration.
- **A preset is data, not code.** Built-in presets have stable ids (`research`,
  `small-local`, `assistant`, `coding`, `blank`) because a chat stores the one it
  was created with; the frontend translates `preset.<id>` and the `Name` field is
  only the fallback.
- **A boolean that costs money or a request ships off.** Hosted backends and image
  servers are the precedent: enabling one by default is a probe nobody asked for.

## Done when

- `make check` is green.
- The setting survives a restart, and an old `kichat.db` without the field opens
  with the default.
- The label appears in all three languages, and `models.ts` was regenerated.
- `git diff` on `frontend/wailsjs/go/models.ts` contains only the new field.
