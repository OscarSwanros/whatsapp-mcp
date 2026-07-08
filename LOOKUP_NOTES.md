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
