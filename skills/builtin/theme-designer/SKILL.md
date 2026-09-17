---
name: theme-designer
description: Author and apply a color theme for the running session with the set_theme tool, persisting it to ~/.pollytool/themes only after the user confirms. Use when the user asks to change polly's colors, match their terminal palette, fix a role that is unreadable, or save a theme for later.
---

# Theme

Restyle the running session by setting colors for polly's fixed role
vocabulary, then persist the theme to a file only after the user agrees.

## 1. Interview first

Do not guess. One topic per message:

- **Background**: light or dark, and should the theme set its own
  `background` or sit on the terminal's? A theme is one look; a user who wants
  both a light and a dark version gets two themes with two names.
- **Match the terminal or pin RGB**: reuse the terminal's own palette slots
  (`palette:N`) and color names, or specify exact colors (`#rrggbb`)?
- **What to change**: only the roles the user complains about, or a full
  restyle? Every role you leave out keeps polly's built-in color.

## 2. Pick from the 25 roles

- **Semantic (7)**: `ok`, `err`, `run`, `accent`, `active`, `muted`, `code`.
- **Token (7)**: `syn-comment`, `syn-keyword`, `syn-string`, `syn-number`,
  `syn-func`, `syn-add`, `syn-del`. Omit a token role to inherit its
  fallback — `syn-comment`→`muted`, `syn-keyword`→`accent`, `syn-string`→`ok`,
  `syn-number`→`active`, `syn-func`→`code` (keeping its bold),
  `syn-add`→`ok`, `syn-del`→`err`. For code and diffs, usually set only the
  semantic role and let the tokens follow it.
- **Bird (9)**: `polly-green`, `polly-light`, `polly-wing`, `polly-crown`,
  `polly-beak`, `polly-mouth`, `polly-face`, `polly-eye`, `polly-foot`.
- **Surface (2)**: `background` fills the whole interface behind everything;
  `foreground` is plain text (the user's input, the model's replies). Both
  default to `inherit`, the terminal's own. Set them as a pair — the
  terminal's default text color may be unreadable on a background it was not
  chosen for — and once you set a background, check every other role for
  contrast against it, `muted` and `code` especially.

## 3. Know the value forms

- `"#rrggbb"` or `"#rgb"` — true color.
- `"palette:N"` with N from 0 to 255 — that ANSI slot.
- A terminal color name such as `"green"`, `"grey"`, or `"darkred"`.
- `"inherit"` — no color, so the terminal's default foreground shows through.
  Rejected for `accent` and `muted`, which must stay distinct real colors.
- Omit the key — polly's built-in color for that role.

Naming another role as a value (for example `"accent": "muted"`) is invalid:
values resolve against the built-in names, not against other roles.

## 4. Apply and show

Call `set_theme` with `name` plus `colors`, but **without** `persist`. The session restyles immediately.
Ask the user to look at the result and iterate until they are happy. Nothing
has been written to disk at this point.

## 5. Persist only on an explicit yes

Call `set_theme` again with `persist: true`. Persist without confirmation
writes nothing and returns
`{"status":"confirmation_required","path":"...","theme":{...}}` — that reply
is the protocol, not an error. Show the user the exact path and the colors you
are about to write, get a clear yes, then call once more with `persist: true`
and `confirm: true`. Add `overwrite: true` only when the user agrees to
replace an existing file (the persisted themes live in
`~/.pollytool/themes/<name>.json`). Never persist on your own initiative.

## 6. Correct yourself from error codes

- `UNKNOWN_ROLE` — a role name is not in the list of 25; fix the typo.
- `INVALID_COLOR` — a value is not one of the forms above, a `palette:N` is
  out of range, a value names another role, or `inherit` is used on `accent`
  or `muted`.
- `INVALID_THEME` — the theme is otherwise malformed; check the name and the
  layer structure you sent.
- `THEME_EXISTS` — the file already exists; ask the user, then retry with
  `overwrite: true`.
- `THEME_WRITE_FAILED` — the write itself failed; report it and do not retry
  blindly.
