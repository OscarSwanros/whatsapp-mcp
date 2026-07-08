# WhatsApp store — lookup notes (operational)

## RULE: never look up a chat/contact by phone number or name alone
WhatsApp multi-device increasingly keys chats by an **opaque LID** — a `chat_jid` like
`210736932511940@lid`, NOT the phone-number jid (`<number>@s.whatsapp.net`). And
`chats.name` is frequently the raw number/LID string, not the person's name.

**Consequence:** searching `messages`/`chats` by phone number OR by `name` will *silently
miss* LID-keyed chats — a DM that plainly exists in the app returns zero rows.

**Find a chat reliably instead:**
1. Search by distinctive **message content** (`messages.content LIKE '%…%'`) → get `chat_jid`.
2. Use iMCP `contacts_search` to resolve number→name for display, but do NOT assume the
   WhatsApp jid contains that number.
3. Group messages carry the individual in `sender`; DMs may be LID-keyed with no number anywhere.

**Also:** linked-device **history sync is partial** — not every chat/message is pushed to a
newly-linked device; historical messages may be absent, and real-time messages arrive only
while the bridge is connected.

_Captured 2026-07-08 after a Miriam-Vera DM lookup missed on both number and name (chat keyed
`210736932511940@lid`), located by content. Refs HMB-327. Graduate into standards/PERSONAL_DOMAIN.md when that scaffolding lands._

## Media download 403 — historical media may be unfetchable
`/api/download` can return **403** for media received via history sync (before the device
linked, or an old message): WhatsApp's CDN won't re-serve expired media references to a
linked device. **Real-time media** (received while the bridge is connected) downloads fine.
Workaround for a specific old note: **forward it in WhatsApp** — that creates fresh media the
bridge can fetch. Captured 2026-07-08 (Miriam voice note 1:35pm, received pre-link → 403).

## The RIGHT lookup — use whatsmeow's live mapping, don't copy it
whatsmeow already maintains the authoritative, LIVE mapping in the SESSION store
(`whatsapp.db`): `whatsmeow_lid_map(lid, pn)` + `whatsmeow_contacts(their_jid, full_name,
push_name, business_name)`. Do NOT snapshot it into a separate table — it drifts. Resolve by
name / number / LID against these live tables. Helper: `wa-find.py "Miriam" --messages 5`.

## CORRECTION to the 403 note above
Real-time media ALSO 403s — the earlier "real-time downloads fine" was wrong. Root cause is a
bridge download-auth bug (it uses the stored CDN url whose oe/oh go stale, instead of
re-deriving creds via whatsmeow's media connection). Fix in progress under HMB-327.
