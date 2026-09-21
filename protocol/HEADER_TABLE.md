# BHTTP/1 Static Header Table

Normative. Companion to `SPEC.md` and `WIRE_FORMAT.md`.

## 1. Table

```text
ID    Name               Defined semantics in v1
──────────────────────────────────────────────────────────────────────
0     (custom follows)   Header name is transmitted explicitly (WIRE_FORMAT §7)
1     content-type       Media type of the body. Informational.
2     content-length     Decimal ASCII digits: total body bytes across all DATA frames.
3     server             Free-form implementation identifier. Informational.
4     date               Name registration only. No value format defined in v1.
5     connection         Name registration only. No behavior defined in v1.
6     content-encoding   Name registration only. No behavior defined in v1.
7     cache-control      Name registration only. No behavior defined in v1.
8     last-modified      Name registration only. No value format defined in v1.
9–255 (unassigned)       Receivers MUST parse and ignore (SPEC §7.4).
```

IDs 4–8 are reserved names so that later versions can attach semantics
without changing the encoding. In v1 they have **no** effect on behavior. In
particular, `connection` never controls connection lifetime: a peer ends a
connection only by closing TCP (SPEC §10).

## 2. Rules

1. A sender MUST serialize a header whose name appears in the table using its
   non-zero ID. It MUST NOT send it as a custom (ID 0) header.
2. A receiver that gets a custom header whose name equals a table name MUST
   treat the message as malformed (`400`, SPEC §8).
3. Names are lowercase ASCII. The table names above are the canonical
   spellings. There is no case-folding on the wire: custom names containing
   uppercase are malformed.
4. Values are byte strings. `content-length` MUST be 1 to 19 ASCII digits
   `0`–`9`, no sign, no whitespace; compared numerically, so leading zeros are
   accepted by receivers, though senders SHOULD NOT emit them. Other values have
   no v1 syntax.
5. The same header (same ID or same custom name) MAY appear more than once.
   Order is preserved. v1 defines no merge semantics.
6. Registry changes: a new ID is added by editing this file and bumping the
   protocol revision. IDs are never reused or reassigned.
