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
chats={d(j):j for j,_ in msg.execute("SELECT jid,name FROM chats")}
seen=set()
for nm,k in cands:
    pn=k if k in pn2lid else (lid2pn.get(k) or k); lid=pn2lid.get(pn) or (k if k in lid2pn else "")
    if (pn,lid) in seen: continue
    seen.add((pn,lid)); cj=chats.get(d(pn)) or chats.get(d(lid))
    print(f"{nm or '(no name)'} | pn={pn or '?'} | lid={lid or '?'} | chat={cj or '(no synced chat)'}")
    if a.messages and cj:
        for ts,fm,mt,c in msg.execute("SELECT timestamp,is_from_me,media_type,substr(content,1,80) FROM messages WHERE chat_jid=? ORDER BY timestamp DESC LIMIT ?",(cj,a.messages)):
            print(f"    {ts} [{'me' if fm else 'them'}] {('['+mt+'] ') if mt else ''}{c or ''}")
msg.close()
