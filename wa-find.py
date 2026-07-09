#!/usr/bin/env python3
"""wa-find — resolve a WhatsApp conversation to its chat_jid.

HMB-336: a NAMED person resolves ONLY via the VERIFIED mapping — the mode-600
`identity.md` name↔number↔LID↔chat_jid table + the `resolved-chats` pins in
`monitoring-scope.md`. wa-find NO LONGER fuzzy-matches a name against the WhatsApp
address book (`whatsmeow_contacts`): that substring match conflated a person's
short nickname with a DIFFERENT contact whose surname merely contained it (and
with every same-first-name contact in the book), and this tool feeds the send
path, where a wrong mapping is a wrong recipient.

Resolution:
  * a NUMBER / LID  → resolved EXACTLY (digit match) against the live
    `whatsmeow_lid_map` + the chat list, self-healed across a LID migration.
  * a NAME / LABEL  → resolved ONLY from the verified mapping. An exact scope
    label (vane / darwin / ague / family / dive-center) wins; otherwise the query
    must match a verified person NAME from identity.md. A name that matches nobody
    verified is REFUSED (never guessed); one that matches several is printed for
    the operator to pick (never auto-collapsed to one).

The live `whatsmeow_lid_map` is read READ-ONLY for the number↔lid self-heal only;
`whatsmeow_contacts` is never consulted. Nothing here sends.

Usage: python3 wa-find.py <name|label|number|lid> [--messages N]
Env overrides: WA_DB, WA_MSG_DB, WA_IDENTITY, WA_SCOPE.
"""
import argparse
import os
import re
import sqlite3
import sys

WA = os.path.expanduser(os.environ.get("WA_DB", "~/.config/homebase/whatsapp/whatsapp.db"))
MSG = os.path.expanduser(os.environ.get("WA_MSG_DB", "~/.config/homebase/whatsapp/messages.db"))
IDENTITY = os.path.expanduser(os.environ.get("WA_IDENTITY", "~/.config/homebase/personal/identity.md"))
SCOPE = os.path.expanduser(os.environ.get("WA_SCOPE", "~/.config/homebase/personal/monitoring-scope.md"))

# Digit run of a jid/number (drops @domain and :device).
d = lambda s: re.sub(r"\D", "", (s or "").split("@")[0].split(":")[0])
# A verified person NAME = two or more capitalised words (Latin-1 + Spanish). This
# cleanly picks person names out of identity.md prose while skipping ALL-CAPS role
# words (WIFE / MOTHER / WORK / MAIN) and lowercase brand/group tokens (abucear).
NAME_RE = re.compile(r"[A-ZÁÉÍÓÚÑ][a-záéíóúñ]+(?:\s+[A-ZÁÉÍÓÚÑ][a-záéíóúñ]+)+")
LABEL_RE = re.compile(r"^##\s+([A-Za-z0-9-]+)")


def parse_identities(path):
    """Parse identity.md → list of (name_lower, label, {digit_run,…}).

    A digit-bearing line's name is its own inline person name when present, else
    the current `## label (Name)` header's person name (covers the ague layout,
    where the name is on the header and the numbers on role bullets). NEVER a fuzzy
    contact lookup — only the operator-verified table."""
    ids = []
    try:
        text = open(path, encoding="utf-8").read()
    except OSError:
        return ids
    label = None
    header_name = None
    for line in text.splitlines():
        hm = LABEL_RE.match(line)
        if hm:
            label = hm.group(1).lower()
            nm = NAME_RE.search(line)
            header_name = nm.group(0) if nm else None
        digits = re.findall(r"\d{7,}", line)
        if not digits or label is None:
            continue
        nm = NAME_RE.search(line)
        name = nm.group(0) if nm else header_name
        if not name:
            continue
        ids.append((name.lower(), label, set(digits)))
    return ids


def parse_label_jids(path):
    """Parse the ```resolved-chats block from monitoring-scope.md → {label: [jids]}."""
    out = {}
    try:
        text = open(path, encoding="utf-8").read()
    except OSError:
        return out
    m = re.search(r"```[ \t]*resolved-chats[ \t]*\n(.*?)\n```", text, re.S | re.IGNORECASE)
    if not m:
        return out
    for line in m.group(1).splitlines():
        line = line.strip()
        if not line or line.startswith("#") or ":" not in line:
            continue
        lab, rhs = line.split(":", 1)
        lab = lab.strip().lower()
        jids = [x.strip() for x in rhs.split(",") if x.strip()]
        if lab and jids:
            out.setdefault(lab, []).extend(jids)
    return out


def load_maps():
    wa = sqlite3.connect(f"file:{WA}?mode=ro", uri=True)
    lid2pn = dict(wa.execute("SELECT lid,pn FROM whatsmeow_lid_map"))
    wa.close()  # whatsmeow_contacts is deliberately NOT read
    pn2lid = {v: k for k, v in lid2pn.items()}
    msg = sqlite3.connect(f"file:{MSG}?mode=ro", uri=True)
    chats = {}
    lmt = {}
    for j, _n, t in msg.execute("SELECT jid,name,last_message_time FROM chats"):
        chats.setdefault(d(j), j)
        lmt[j] = t or ""
    msg.close()
    return lid2pn, pn2lid, chats, lmt


def chats_for_digits(seed_digits, pn2lid, lid2pn, chats):
    """Map a set of digit-runs to their chats, self-healing pn↔lid across a LID
    migration (enrol both the pn chat and the lid chat of the SAME identity)."""
    out = []
    for k in seed_digits:
        pn = k if k in pn2lid else (lid2pn.get(k) or k)
        lid = pn2lid.get(pn) or (k if k in lid2pn else "")
        for dd in (d(pn), d(lid), d(k)):
            cj = chats.get(dd)
            if cj and cj not in out:
                out.append(cj)
    return out


def emit_chats(name, label, cand_chats, lmt, messages, msg_db):
    if not cand_chats:
        print(f"{name} [{label}] | (no synced chat for the verified jids)")
        return
    cand_chats = sorted(cand_chats, key=lambda c: lmt.get(c, ""), reverse=True)
    for i, cj in enumerate(cand_chats):
        state = "active" if i == 0 else "stale"
        print(f"{name} [{label}] | chat={cj} [{state} {lmt.get(cj,'?')}]")
    if messages:
        active = cand_chats[0]
        m = sqlite3.connect(f"file:{msg_db}?mode=ro", uri=True)
        for ts, fm, mt, c in m.execute(
                "SELECT timestamp,is_from_me,media_type,substr(content,1,80) "
                "FROM messages WHERE chat_jid=? ORDER BY timestamp DESC LIMIT ?",
                (active, messages)):
            print(f"    {ts} [{'me' if fm else 'them'}] {('['+mt+'] ') if mt else ''}{c or ''}")
        m.close()


def main():
    p = argparse.ArgumentParser()
    p.add_argument("query")
    p.add_argument("--messages", type=int, default=0)
    a = p.parse_args()
    ql = a.query.strip().lower()
    qd = d(a.query)

    lid2pn, pn2lid, chats, lmt = load_maps()

    # NUMBER / LID mode — exact digit resolution, self-healed. Not name matching.
    if qd and len(qd) >= 7:
        cand = chats_for_digits({qd}, pn2lid, lid2pn, chats)
        emit_chats("(number)", "id", cand, lmt, a.messages, MSG)
        return 0

    # NAME / LABEL mode — VERIFIED mapping only.
    label_jids = parse_label_jids(SCOPE)
    ids = parse_identities(IDENTITY)

    # 1) exact scope label wins (vane / darwin / ague / family / dive-center).
    if ql in label_jids:
        cand = chats_for_digits({d(j) for j in label_jids[ql]}, pn2lid, lid2pn, chats)
        emit_chats(f"(label {ql})", ql, cand, lmt, a.messages, MSG)
        return 0

    # 2) verified person-name match (exact or substring within the verified names).
    matches = [(nm, lab, digs) for nm, lab, digs in ids if ql == nm or ql in nm]
    if not matches:
        print(f"wa-find: '{a.query}' is not in the verified identity mapping "
              f"(identity.md / resolved-chats).", file=sys.stderr)
        print("         Name-substring matching against the address book is DISABLED "
              "(HMB-336: it conflated a nickname with a different contact's surname).",
              file=sys.stderr)
        print("         Query by a verified scope label, the person's full verified "
              "name, or a number/LID.", file=sys.stderr)
        return 4

    labels = {lab for _, lab, _ in matches}
    if len(matches) > 1:
        note = "AMBIGUOUS across labels" if len(labels) > 1 else "multiple matches"
        print(f"wa-find: '{a.query}' → {note}; printing each — pick the intended jid "
              f"(never auto-collapsed):", file=sys.stderr)
    for nm, lab, digs in matches:
        cand = chats_for_digits(digs, pn2lid, lid2pn, chats)
        emit_chats(nm, lab, cand, lmt, a.messages, MSG)
    return 0


if __name__ == "__main__":
    sys.exit(main())
