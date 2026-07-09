#!/usr/bin/env python3
"""wa-find — resolve a WhatsApp conversation by name / phone / LID using the LIVE
whatsmeow mapping (whatsmeow_lid_map + whatsmeow_contacts in the session store).
No separate table is maintained; always reads the authoritative source.
Usage: python3 wa-find.py <name|number|lid> [--messages N]"""
import sqlite3, os, re, argparse
WA=os.path.expanduser("~/.config/homebase/whatsapp/whatsapp.db")
MSG=os.path.expanduser("~/.config/homebase/whatsapp/messages.db")
d=lambda s: re.sub(r'\D','',(s or '').split('@')[0].split(':')[0])
p=argparse.ArgumentParser(); p.add_argument("query"); p.add_argument("--messages",type=int,default=0)
a=p.parse_args(); ql=a.query.lower(); qd=d(a.query)
wa=sqlite3.connect(f"file:{WA}?mode=ro",uri=True)
lid2pn=dict(wa.execute("SELECT lid,pn FROM whatsmeow_lid_map")); pn2lid={v:k for k,v in lid2pn.items()}
cands=[]
for their,full,push,biz in wa.execute("SELECT their_jid,full_name,push_name,business_name FROM whatsmeow_contacts"):
    nm=" / ".join(x for x in (full,push,biz) if x); k=d(their)
    if (ql and ql in nm.lower()) or (qd and qd==k): cands.append((nm,k))
wa.close()
if not cands and qd: cands=[("(id)",qd)]
msg=sqlite3.connect(f"file:{MSG}?mode=ro",uri=True)
# Key chats by digit-run, keeping last_message_time so we can prefer the ACTIVE chat.
chats={}; lmt={}
for j,_n,t in msg.execute("SELECT jid,name,last_message_time FROM chats"):
    chats.setdefault(d(j),j); lmt[j]=t or ""
seen=set()
for nm,k in cands:
    pn=k if k in pn2lid else (lid2pn.get(k) or k); lid=pn2lid.get(pn) or (k if k in lid2pn else "")
    if (pn,lid) in seen: continue
    seen.add((pn,lid))
    # THE LID FIX (HMB-333): a contact often has BOTH a phone-jid chat
    # (<pn>@s.whatsapp.net) AND a LID chat (<lid>@lid). Don't lock onto the phone
    # chat (the old `chats.get(d(pn)) or chats.get(d(lid))` did) — collect BOTH and
    # PREFER the one with the most recent activity, so a conversation that migrated
    # to the LID (e.g. Alfonso: active LID 123540623896764@lid, stale phone
    # 5213328357409@s.whatsapp.net) is reported as ACTIVE, not the dead phone chat.
    cand_chats=[]
    for c in (chats.get(d(pn)), chats.get(d(lid))):
        if c and c not in cand_chats: cand_chats.append(c)
    cand_chats.sort(key=lambda c: lmt.get(c,""), reverse=True)  # most-recent first = ACTIVE
    if cand_chats:
        for i,cj in enumerate(cand_chats):
            print(f"{nm or '(no name)'} | pn={pn or '?'} | lid={lid or '?'} | chat={cj} [{'active' if i==0 else 'stale'} {lmt.get(cj,'?')}]")
    else:
        print(f"{nm or '(no name)'} | pn={pn or '?'} | lid={lid or '?'} | chat=(no synced chat)")
    active=cand_chats[0] if cand_chats else None
    if a.messages and active:
        for ts,fm,mt,c in msg.execute("SELECT timestamp,is_from_me,media_type,substr(content,1,80) FROM messages WHERE chat_jid=? ORDER BY timestamp DESC LIMIT ?",(active,a.messages)):
            print(f"    {ts} [{'me' if fm else 'them'}] {('['+mt+'] ') if mt else ''}{c or ''}")
msg.close()
