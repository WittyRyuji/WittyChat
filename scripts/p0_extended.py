#!/usr/bin/env python3
"""P0 extended coverage — features added after smoke_p0.py:
profile/avatar, groups, message edit/recall/quote/mention, multi-attachment +
placeholder bodies, client thumbnails, object ACL, mail flags/move/delete/
reply/forward/attachment-ACL, admin objects+purge, assistants, sync/unread.

Usage: python scripts/p0_extended.py   (server on :8080, worker running)
"""
import io, json, os, sys, time, uuid, urllib.request, urllib.error

BASE = "http://localhost:8080"
DATA = os.path.join(os.path.dirname(__file__), "..", "data")
passed, failed = [], []


def req(method, path, body=None, token=None, raw=None, ctype="application/json"):
    data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
    r = urllib.request.Request(BASE + path, data=data, method=method)
    if data is not None:
        r.add_header("Content-Type", ctype)
    if token:
        r.add_header("Authorization", f"Bearer {token}")
    try:
        resp = urllib.request.urlopen(r, timeout=15)
        return resp.status, resp.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def check(name, ok, detail=""):
    (passed if ok else failed).append(name)
    print(("PASS" if ok else "FAIL"), name, detail)


def login(email, pw):
    st, b = req("POST", "/api/v1/auth/login", {"email": email, "password": pw,
                                             "device_name": "p0x", "device_kind": "web"})
    return json.loads(b)["session_token"] if st == 200 else None


def upload(token, conv, name, mime, data):
    bd = "b" + uuid.uuid4().hex
    mp = (f"--{bd}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"{name}\"\r\n"
          f"Content-Type: {mime}\r\n\r\n").encode() + data + f"\r\n--{bd}--\r\n".encode()
    st, b = req("POST", f"/api/v1/conversations/{conv}/attachments", token=token,
                raw=mp, ctype=f"multipart/form-data; boundary={bd}")
    return json.loads(b)["id"] if st in (200, 201) else None


def upload_purpose(token, purpose, name, mime, data):
    bd = "b" + uuid.uuid4().hex
    mp = (f"--{bd}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"{name}\"\r\n"
          f"Content-Type: {mime}\r\n\r\n").encode() + data + f"\r\n--{bd}--\r\n".encode()
    st, b = req("POST", f"/api/v1/objects/upload?purpose={purpose}", token=token,
                raw=mp, ctype=f"multipart/form-data; boundary={bd}")
    return (st, json.loads(b) if st in (200, 201) else b)


def send_msg(token, conv, markdown="", obj_ids=None, quote=0, mention_all=False):
    body = {"client_msg_id": str(uuid.uuid4()), "markdown": markdown}
    if obj_ids:
        body["object_ids"] = obj_ids
    if quote:
        body["quote_op_seq"] = quote
    if mention_all:
        body["mention_all"] = True
    return req("POST", f"/api/v1/conversations/{conv}/messages", body, token)


def get_ops(token, conv, after=0):
    st, b = req("GET", f"/api/v1/conversations/{conv}/ops?after={after}", None, token)
    return json.loads(b)["ops"] if st == 200 else []


def folders(token):
    st, b = req("GET", "/api/v1/mail/folders", None, token)
    return {f["kind"]: f for f in json.loads(b)} if st == 200 else {}


def mail_list(token, folder_id):
    st, b = req("GET", f"/api/v1/mail/messages?folder={folder_id}", None, token)
    return json.loads(b) if st == 200 else []


# 1x1 px JPEG (valid, tiny) for thumbnail tests
JPEG = bytes.fromhex(
    "ffd8ffe000104a46494600010100000100010000ffdb00430003020203020203030303040303"
    "0405080504040a070607080c0a0c0c0b0a0b0b0d0e12100d0e110e0b0b101610111314151515"
    "0c0f171816141812141514ffdb00430103040405040509050509140d0b0d1414141414141414"
    "1414141414141414141414141414141414141414141414141414141414141414141414141414"
    "14141414ffc00011080001000103012200021101031101ffc400140001000000000000000000"
    "0000000000000008ffc4001410010000000000000000000000000000000000ffda000c030100"
    "02110311003f00b2c001ffd9")

admin = login("admin@wittyloong.com", "DevPass123!")
alice = login("alice@wittyloong.com", "AlicePass123!")
check("login admin+alice", admin and alice)
if not (admin and alice):
    sys.exit(1)

# --- fresh user via admin provisioning ---
email = f"p0x-{uuid.uuid4().hex[:8]}@wittyloong.com"
st, b = req("POST", "/api/v1/admin/users", {"email": email, "display_name": "P0X"}, admin)
act = json.loads(b)["activation_url"]
from urllib.parse import urlparse, parse_qs
tok = parse_qs(urlparse(act).query).get("token", [act.rsplit("/", 1)[-1]])[0]
req("POST", "/api/v1/auth/activate", {"token": tok, "password": "P0xPass123!"})
p0x = login(email, "P0xPass123!")
check("provision+activate+login", p0x is not None)

st, b = req("GET", "/api/v1/auth/me", None, p0x)
p0x_id = json.loads(b)["user"]["id"]
st, b = req("GET", "/api/v1/auth/me", None, alice)
alice_id = json.loads(b)["user"]["id"]
st, b = req("GET", "/api/v1/auth/me", None, admin)
admin_id = json.loads(b)["user"]["id"]

# ================= profile =================
new_name = "P0X-Renamed"
st, b = req("PATCH", "/api/v1/users/me", {"display_name": new_name}, p0x)
check("rename profile", st == 200 and new_name.encode() in b, str(st))
st, b = upload_purpose(p0x, "avatar", "a.jpg", "image/jpeg", JPEG)
check("avatar upload", st in (200, 201), str(st))
av_id = b["id"]
st, b = req("PATCH", "/api/v1/users/me", {"avatar_object_id": av_id}, p0x)
check("bind avatar", st == 200, str(st))
st, b = req("GET", f"/api/v1/users/{p0x_id}/avatar", None, admin)
check("peer sees avatar", st == 200 and b[:2] == b"\xff\xd8", str(st))
st, b = req("GET", f"/api/v1/users/{p0x_id}/avatar?token={p0x}", None)
check("avatar via ?token=", st == 200, str(st))
# someone else's object can't be bound as avatar
st, b = upload_purpose(alice, "avatar", "x.jpg", "image/jpeg", JPEG)
al_av = b["id"]
st, b = req("PATCH", "/api/v1/users/me", {"avatar_object_id": al_av}, p0x)
check("avatar ownership enforced", st == 403, str(st))

# ================= groups =================
st, b = req("POST", "/api/v1/conversations/group",
            {"title": f"p0x-pub-{uuid.uuid4().hex[:4]}", "is_public": True,
             "member_ids": [alice_id]}, admin)
check("create public group", st in (200, 201), str(st))
grp = json.loads(b)["id"] if st in (200, 201) else None
gtitle = json.loads(b)["title"]
st, b = req("GET", f"/api/v1/groups/public?q={gtitle[:8]}", None, p0x)
hits = json.loads(b).get("groups", [])
check("search public group", any(g["id"] == grp for g in hits), str(len(hits)))
st, b = req("POST", f"/api/v1/groups/{grp}/join", None, p0x)
check("join public group", st in (200, 204), str(st))
st, b = req("GET", f"/api/v1/conversations/{grp}/members", None, p0x)
mems = json.loads(b).get("members", json.loads(b) if isinstance(json.loads(b), list) else [])
check("member list includes joiner", any(m.get("user_id") == p0x_id for m in mems), str(len(mems)))
st, b = req("POST", f"/api/v1/conversations/{grp}/rename", {"title": gtitle + "-r"}, admin)
check("owner renames group", st in (200, 201), str(st))
st, b = req("POST", f"/api/v1/conversations/{grp}/rename", {"title": "nope"}, p0x)
check("non-owner rename denied", st in (400, 403), str(st))

# ================= messaging =================
st, b = req("POST", "/api/v1/conversations/dm", {"user_id": p0x_id}, alice)
conv = json.loads(b)["id"]
check("alice-p0x dm", conv is not None, str(st))

st, b = send_msg(alice, conv, "**hello** `p0x`")
seq = json.loads(b)["op_seq"]
check("send markdown", st == 201, str(st))
st, b = req("POST", f"/api/v1/conversations/{conv}/messages/{seq}/edit",
            {"markdown": "edited body"}, alice)
check("edit accepted", st in (200, 201), str(st))
ops = get_ops(p0x, conv)
ed = [o for o in ops if o["op_seq"] == seq]
check("edit merged into view", ed and ed[0]["body"] == "edited body" and ed[0].get("edited"),
      ed[0]["body"] if ed else "missing")
st, b = send_msg(alice, conv, "reply here", quote=seq)
check("quoted reply", st == 201 and b'"quote_op_seq"' in b, str(st))
st, b = send_msg(p0x, conv, "ping @all", mention_all=True)
check("mention_all", st == 201, str(st))

# recall
st, b = send_msg(alice, conv, "oops recall me")
rseq = json.loads(b)["op_seq"]
st, b = req("POST", f"/api/v1/conversations/{conv}/messages/{rseq}/recall", None, alice)
check("recall own msg", st in (200, 201, 204), str(st))
ops = get_ops(p0x, conv)
rc = [o for o in ops if o["op_seq"] == rseq]
check("peer sees recalled state", rc and rc[0].get("recalled") is True,
      json.dumps(rc[0].get("recalled")) if rc else "missing")

# ================= reactions (👍 like) =================
st, b = send_msg(p0x, conv, "like this msg")
lseq = json.loads(b)["op_seq"]
st, b = req("POST", f"/api/v1/conversations/{conv}/messages/{lseq}/reaction",
            {"emoji": "👍", "add": True}, alice)
check("like a peer msg", st in (200, 201), str(st))
ops = get_ops(p0x, conv)
lm = [o for o in ops if o["op_seq"] == lseq][0]
rx = lm["payload"].get("reactions") or []
thumbs = [r for r in rx if r["emoji"] == "👍"]
check("reaction aggregated on msg",
      thumbs and len(thumbs[0]["users"]) == 1
      and thumbs[0]["users"][0]["id"] == alice_id,
      json.dumps(rx, ensure_ascii=False))
st, b = req("POST", f"/api/v1/conversations/{conv}/messages/{lseq}/reaction",
            {"emoji": "👍", "add": True}, p0x)
check("second liker", st in (200, 201), str(st))
ops = get_ops(alice, conv)
rx2 = [o for o in ops if o["op_seq"] == lseq][0]["payload"].get("reactions") or []
check("two likers counted",
      any(r["emoji"] == "👍" and len(r["users"]) == 2 for r in rx2),
      json.dumps(rx2, ensure_ascii=False))
rops = [o for o in ops if o.get("kind") == "reaction" and o.get("target_seq") == lseq]
check("reaction op carries full aggregate",
      bool(rops) and bool(rops[-1]["payload"].get("reactions_full")),
      str(len(rops)))
# idempotent re-add: p0x's latest is already add → op_seq 0, no new op
st, b = req("POST", f"/api/v1/conversations/{conv}/messages/{lseq}/reaction",
            {"emoji": "👍", "add": True}, p0x)
check("repeat add is no-op", json.loads(b).get("op_seq", -1) == 0, str(st) + b[:60].decode(errors="replace"))
st, b = req("POST", f"/api/v1/conversations/{conv}/messages/{lseq}/reaction",
            {"emoji": "👍", "add": False}, alice)
check("unlike own", st in (200, 201), str(st))
ops = get_ops(alice, conv)
rx3 = [o for o in ops if o["op_seq"] == lseq][0]["payload"].get("reactions") or []
like3 = [r for r in rx3 if r["emoji"] == "👍"]
check("unlike leaves one",
      like3 and len(like3[0]["users"]) == 1 and like3[0]["users"][0]["id"] == p0x_id,
      json.dumps(rx3, ensure_ascii=False))
st, b = req("POST", f"/api/v1/conversations/{conv}/messages/999999/reaction",
            {"emoji": "👍", "add": True}, alice)
check("react missing target 404", st == 404, str(st))
st, b = req("POST", f"/api/v1/conversations/{conv}/messages/{lseq}/reaction",
            {"emoji": "", "add": True}, alice)
check("empty emoji 400", st in (400, 422), str(st))

# ================= multi-attachment + placeholders =================
img1 = upload(alice, conv, "i1.jpg", "image/jpeg", JPEG + uuid.uuid4().bytes[:8])
img2 = upload(alice, conv, "i2.jpg", "image/jpeg", JPEG + uuid.uuid4().bytes[:8])
txt = upload(alice, conv, "n.txt", "text/plain", b"note " + uuid.uuid4().bytes[:6])
check("3 uploads", img1 and img2 and txt)

st, b = send_msg(alice, conv, "", [img1])
check("pure image msg", st == 201, str(st))
iseq = json.loads(b)["op_seq"]
ops = get_ops(p0x, conv, after=iseq - 1)
o = [x for x in ops if x["op_seq"] == iseq][0]
check("image-only placeholder is [图片]", o["body"] == "[图片]", o["body"])
check("attachments array in payload",
      len(o["payload"]["attachments"]) == 1, str(len(o["payload"]["attachments"])))

st, b = send_msg(alice, conv, "", [img1, img2, txt])
check("multi-attachment msg", st == 201, str(st))
mseq = json.loads(b)["op_seq"]
ops = get_ops(p0x, conv, after=mseq - 1)
o = [x for x in ops if x["op_seq"] == mseq][0]
check("mixed placeholder is [附件]", o["body"] == "[附件]", o["body"])
check("payload carries all 3 attachments",
      len(o["payload"]["attachments"]) == 3, str(len(o["payload"]["attachments"])))

st, b = send_msg(alice, conv, "cap", [img2])
oseq = json.loads(b)["op_seq"]
ops = get_ops(p0x, conv, after=oseq - 1)
o = [x for x in ops if x["op_seq"] == oseq][0]
check("text+image keeps real body", o["body"] == "cap", o["body"])

# ================= object ACL + client thumb =================
st, b = req("GET", f"/api/v1/objects/{txt}/download", None, admin)
check("non-member download denied", st == 403, str(st))
st, b = req("GET", f"/api/v1/objects/{txt}/download?token={p0x}", None)
check("member download via ?token=", st == 200, str(st))
# client thumbnail upload + serve
bd = "b" + uuid.uuid4().hex
mp = (f"--{bd}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"t.jpg\"\r\n"
      f"Content-Type: image/jpeg\r\n\r\n").encode() + JPEG + f"\r\n--{bd}--\r\n".encode()
st, b = req("POST", f"/api/v1/objects/{img1}/thumb", token=alice,
            raw=mp, ctype=f"multipart/form-data; boundary={bd}")
check("client thumb upload (owner)", st in (200, 201, 204), str(st))
st, b = req("POST", f"/api/v1/objects/{img1}/thumb", token=p0x,
            raw=mp, ctype=f"multipart/form-data; boundary={bd}")
check("thumb upload non-owner denied", st == 403, str(st))

# sticker flow: object uploaded WITHOUT conv binding, then sent as an
# attachment into the conv — members read it via attachment_refs, not
# object-level conv_id (this was the broken-GIF-on-recipient bug).
st, sobj = upload_purpose(alice, "sticker", "st.gif", "image/gif",
                         b"GIF89a" + uuid.uuid4().bytes[:16])
check("sticker upload (no conv)", st == 200 and sobj.get("id"), str(st))
soid = sobj["id"]
st, b = req("GET", f"/api/v1/objects/{soid}/download?token={p0x}", None)
check("sticker denied before send", st == 403, str(st))
st, b = send_msg(alice, conv, "", [soid])
check("sticker send as attachment", st == 201, str(st))
st, b = req("GET", f"/api/v1/objects/{soid}/download?token={p0x}", None)
check("sticker member download via refs", st == 200 and b.startswith(b"GIF89a"), str(st))
st, b = req("GET", f"/api/v1/objects/{soid}/download?token={admin}", None)
check("sticker still denied to outsider", st == 403, str(st))
st, b = req("GET", f"/api/v1/objects/{img1}/thumb?token={p0x}", None)
check("peer reads thumb", st == 200 and b[:2] == b"\xff\xd8", str(st))
st, b = req("GET", f"/api/v1/objects/{img1}/thumb", None, admin)
check("non-member thumb denied", st == 403, str(st))

# date-partitioned storage on disk — upload unique bytes so dedup can't
# skip the write, then derive the expected partition from the object's own
# uuidv7 ms timestamp (server partitions by UTC date, not local date).
st, uniq_obj = upload_purpose(p0x, "chat", f"p0x-part-{uuid.uuid4().hex}.bin",
                              "application/octet-stream", uuid.uuid4().bytes)
check("unique upload for partition check", st == 200, str(st))
ms = int(uniq_obj["id"].replace("-", "")[:12], 16)  # uuidv7 top 48 bits = unix ms
obj_dir = os.path.join(DATA, "objects", *time.strftime("%Y/%m/%d", time.gmtime(ms / 1000)).split("/"))
check("date-partitioned object dir exists", os.path.isdir(obj_dir), obj_dir)

# ================= mail: full lifecycle =================
f_a = folders(alice)
f_p = folders(p0x)
check("folders both sides", "inbox" in f_a and "inbox" in f_p)

mail_att = ("mail-attachment-body-" + uuid.uuid4().hex).encode()
mst, mb = upload_purpose(alice, "mail", "doc.txt", "text/plain", mail_att)
mobj = mb["id"]
st, b = req("POST", "/api/v1/mail/send",
            {"to": [email], "subject": "P0X-Lifecycle", "body": "see attached",
             "object_ids": [mobj]}, alice)
check("mail send w/ attachment", st == 201, str(st))
time.sleep(0.4)
inbox = mail_list(p0x, f_p["inbox"]["id"])
hit = [m for m in inbox if m["subject"] == "P0X-Lifecycle"]
check("mail in recipient inbox", len(hit) == 1)
mid = hit[0]["id"]

# sent copy must not inflate sender's unread badge (direction='in' only)
f_a2 = folders(alice)
sent_unread = f_a2.get("sent", {}).get("unread", 0)
check("sent folder unread stays 0", sent_unread == 0, f"unread={sent_unread}")

# offset pagination: page 2 shifts the window (regardless of page contents)
st, b1 = req("GET", f"/api/v1/mail/messages?folder={f_p['inbox']['id']}&offset=0", None, p0x)
st2, b2 = req("GET", f"/api/v1/mail/messages?folder={f_p['inbox']['id']}&offset=1", None, p0x)
p1 = json.loads(b1); p2 = json.loads(b2)
check("mail offset pagination", st == 200 and st2 == 200 and
      (len(p1) < 2 or p1[1]["id"] == p2[0]["id"]),
      f"p1={len(p1)} p2={len(p2)}")
st, b = req("GET", f"/api/v1/mail/messages/{mid}", None, p0x)
mm = json.loads(b)
check("mail body + attachment meta",
      mm.get("text_body") == "see attached" and len(mm.get("attachments", [])) == 1,
      str(len(mm.get("attachments", []))))

# recipient attachment ACL (mail path, not conv member)
st, b = req("GET", f"/api/v1/objects/{mobj}/download?token={p0x}", None)
check("mail recipient downloads attachment", st == 200 and b == mail_att, f"{st} {b[:60]!r}")
st, b = req("GET", f"/api/v1/objects/{mobj}/download?token={admin}", None)
check("unrelated user denied", st == 403, str(st))

# flags: unstar -> star, read->unread
st, b = req("POST", f"/api/v1/mail/messages/{mid}/flags", {"starred": True, "read": False}, p0x)
check("star+unread flags", st in (200, 204), str(st))
st, b = req("GET", f"/api/v1/mail/messages/{mid}", None, p0x)
mm = json.loads(b)
check("flags persisted", mm.get("is_starred") is True and mm.get("is_read") is False,
      f"star={mm.get('is_starred')} read={mm.get('is_read')}")

# bulk mark-all-read on the inbox folder (right-click "全部已读" backend)
st, b = req("POST", f"/api/v1/mail/folders/{f_p['inbox']['id']}/read-all", None, p0x)
check("mail folder read-all", st == 200, str(st))
st, b = req("GET", f"/api/v1/mail/messages/{mid}", None, p0x)
check("mail read-all applied", json.loads(b).get("is_read") is True, str(st))
# outsider cannot mark another user's folder
st, b = req("POST", f"/api/v1/mail/folders/{f_p['inbox']['id']}/read-all", None, alice)
check("mail read-all denied to stranger", st in (403, 404), str(st))

# move: inbox -> spam -> inbox -> trash -> purge
for kind, expect in [("spam", True), ("inbox", True), ("trash", True)]:
    st, b = req("POST", f"/api/v1/mail/messages/{mid}/move",
                {"folder": f_p[kind]["id"]}, p0x)
    check(f"move -> {kind}", st in (200, 204), str(st))
    if expect:
        in_kind = mail_list(p0x, f_p[kind]["id"])
        check(f"visible in {kind}", any(m["id"] == mid for m in in_kind))
st, b = req("DELETE", f"/api/v1/mail/messages/{mid}", None, p0x)
check("permanent delete", st == 204, str(st))
st, b = req("GET", f"/api/v1/mail/messages/{mid}", None, p0x)
check("deleted mail 404", st == 404, str(st))

# reply threading + forward attachment reuse
st, b = req("POST", "/api/v1/mail/send",
            {"to": ["alice@wittyloong.com"], "subject": "thread-root", "body": "root"}, p0x)
time.sleep(0.4)
root = [m for m in mail_list(alice, f_a["inbox"]["id"]) if m["subject"] == "thread-root"][0]
st, b = req("GET", f"/api/v1/mail/messages/{root['id']}", None, alice)
root_msg = json.loads(b)
st, b = req("POST", "/api/v1/mail/send",
            {"to": [email], "subject": "Re: thread-root", "body": "reply body",
             "in_reply_to": root_msg.get("message_id", "") or root_msg.get("id", "")}, alice)
check("reply send", st == 201, str(st))
# forward: reuse original attachment object id
st, b = req("POST", "/api/v1/mail/send",
            {"to": ["admin@wittyloong.com"], "subject": "Fwd: attach", "body": "fwd",
             "object_ids": [mobj]}, alice)
check("forward w/ reused attachment", st == 201, str(st))
f_ad = folders(admin)
fwd = []
for _ in range(10):
    time.sleep(0.4)
    fwd = [m for m in mail_list(admin, f_ad["inbox"]["id"]) if m["subject"] == "Fwd: attach"]
    if fwd:
        break
check("forward delivered", len(fwd) >= 1, str(len(fwd)))
if fwd:
    st, b = req("GET", f"/api/v1/mail/messages/{fwd[0]['id']}", None, admin)
    mm = json.loads(b)
    check("fwd attachment readable by new recipient",
          len(mm.get("attachments", [])) == 1, str(len(mm.get("attachments", []))))
    st, b = req("GET", f"/api/v1/objects/{mobj}/download?token={admin}", None)
    check("fwd recipient ACL download", st == 200, str(st))

st, b = req("GET", "/api/v1/mail/addresses", None, p0x)
addrs = json.loads(b)
check("mail addresses auto-provisioned", any(a.get("address") == email for a in addrs),
      str(len(addrs)))

# ================= admin: objects + purge =================
st, b = req("GET", "/api/v1/admin/objects", None, admin)
objs = json.loads(b).get("objects", [])
check("admin object list", st == 200 and len(objs) > 0, str(len(objs)))
check("objects carry owner", all(o.get("owner") and o.get("owner_email") for o in objs[:5]))
st, b = req("GET", f"/api/v1/admin/objects?owner=alice@wittyloong.com&from=2000-01-01&to=2100-01-01",
            None, admin)
objs_a = json.loads(b).get("objects", [])
check("filter by owner", st == 200 and all(o.get("owner_email") == "alice@wittyloong.com" for o in objs_a), str(len(objs_a)))
st, b = req("GET", "/api/v1/admin/objects?owner=nobody@x.com", None, admin)
check("unknown owner 404", st == 404, str(st))
st, b = req("POST", "/api/v1/admin/objects/purge",
            {"owner_email": "alice@wittyloong.com", "day": "1999-01-01"}, admin)
check("purge empty day", st == 200 and json.loads(b).get("deleted") == 0, b[:60])
st, b = req("POST", "/api/v1/admin/objects/purge", {"owner_email": "", "day": "not-a-day"}, admin)
check("bad day 400", st == 400, str(st))
st, b = req("GET", "/api/v1/admin/objects", None, p0x)
check("admin objects denied for user", st in (401, 403), str(st))

# settings get/patch roundtrip (non-destructive key)
st, b = req("GET", "/api/v1/admin/settings", None, admin)
st2, b2 = req("GET", "/api/v1/settings", None, p0x)
check("admin+user settings", st == 200 and st2 == 200, f"{st}/{st2}")

# ================= assistants =================
st, b = req("POST", "/api/v1/assistants",
            {"name": "p0x-bot", "runtime": "acp", "adapter": "stdio",
             "model": "claude", "instructions": "test", "scopes": "[]",
             "session_level": 1, "auto_send": False}, p0x)
check("assistant create", st == 201, str(st))
aid = json.loads(b).get("id") if st == 201 else None
st, b = req("GET", "/api/v1/assistants", None, p0x)
check("assistant list", any(a.get("id") == aid for a in json.loads(b).get("assistants", [])))
st, b = req("PUT", f"/api/v1/assistants/{aid}",
            {"name": "p0x-bot-2", "runtime": "acp", "adapter": "stdio",
             "model": "claude", "instructions": "t2", "scopes": "[]",
             "session_level": 1, "auto_send": False}, p0x)
check("assistant update", st == 200, str(st))
st, b = req("GET", "/api/v1/assistants", None, admin)
check("assistants private per user",
      all(a.get("id") != aid for a in (json.loads(b).get("assistants") or [])))
st, b = req("DELETE", f"/api/v1/assistants/{aid}", None, p0x)
check("assistant delete", st in (200, 204), str(st))
st, b = req("GET", "/api/v1/approvals", None, p0x)
check("approvals list", st == 200, str(st))

# ================= sync / unread / search =================
st, b = req("GET", "/api/v1/unread", None, p0x)
check("unread aggregate", st == 200 and b"total" in b + b"unread" + b"conversations", str(st))
st, b = req("GET", "/api/v1/search/messages?q=edited", None, p0x)
hits = json.loads(b).get("hits", [])
check("message search", st == 200 and len(hits) >= 1, str(len(hits)))
ops1 = get_ops(p0x, conv, after=0)
ops2 = get_ops(p0x, conv, after=1)
check("ops watermark delta", len(ops2) < len(ops1), f"{len(ops1)}->{len(ops2)}")

st, b = req("GET", "/api/v1/sessions", None, p0x)
check("session list", st == 200 and len(json.loads(b).get("sessions", json.loads(b))) >= 1, str(st))
st, b = req("POST", "/api/v1/auth/login", {"email": email, "password": "wrong"})
check("wrong password rejected", st == 401, str(st))

# ================= workspace logo =================
import struct as _st, zlib as _zl
def _png():
    w = h = 32
    raw = b"".join(b"\x00" + b"\x11\x22\x33" * w for _ in range(h))
    def _c(t, d):
        return _st.pack(">I", len(d)) + t + d + _st.pack(">I", _zl.crc32(t + d) & 0xFFFFFFFF)
    return (b"\x89PNG\r\n\x1a\n" + _c(b"IHDR", _st.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0))
            + _c(b"IDAT", _zl.compress(raw)) + _c(b"IEND", b""))

st, logo_obj = upload_purpose(admin, "workspace_logo", f"p0x-logo-{uuid.uuid4().hex[:6]}.png", "image/png", _png())
check("logo upload", st == 200, str(st))
logo_id = logo_obj.get("id") if isinstance(logo_obj, dict) else None
st, b = req("PUT", "/api/v1/admin/settings",
            {"settings": {"workspace.logo_object_id": logo_id}}, admin)
check("logo set admin", st in (200, 204), str(st))
st, b = req("PUT", "/api/v1/admin/settings",
            {"settings": {"workspace.logo_object_id": "x"}}, p0x)
check("logo set non-admin denied", st == 403, str(st))
st, b = req("GET", "/api/v1/settings", None, p0x)
check("effective settings exposes logo",
      st == 200 and json.loads(b).get("workspace", {}).get("logo_object_id") == logo_id, str(st))
st, b = req("GET", "/api/v1/workspace/logo", None, p0x)
check("logo readable by member", st == 200 and b.startswith(b"\x89PNG"), str(st))
st, b = req("GET", "/api/v1/workspace/logo", None, None)
check("logo requires auth", st == 401, str(st))
st, b = req("PUT", "/api/v1/admin/settings",
            {"settings": {"workspace.logo_object_id": ""}}, admin)
check("logo clear", st in (200, 204), str(st))
st, b = req("GET", "/api/v1/workspace/logo", None, admin)
check("logo default after clear", st == 200 and b.startswith(b"\x89PNG"), str(st))

# ---- user prefs (per-user, cross-device) ----
st, b = req("GET", "/api/v1/me/prefs", None, alice)
check("prefs get empty", st == 200 and isinstance(json.loads(b).get("prefs"), dict), str(st))
st, b = req("GET", "/api/v1/me/prefs", None, None)
check("prefs require auth", st == 401, str(st))
st, b = req("PATCH", "/api/v1/me/prefs",
            {"prefs": {"pref.mail.autoimg": False, "pref.theme": "dark",
                       "pref.mail.trusted": ["a@x.com", "b@y.org"]}}, alice)
p = json.loads(b).get("prefs", {}) if st == 200 else {}
check("prefs merge", st == 200 and p.get("pref.theme") == "dark"
      and p.get("pref.mail.autoimg") is False
      and p.get("pref.mail.trusted") == ["a@x.com", "b@y.org"], str(st))
st, b = req("GET", "/api/v1/me/prefs", None, alice)
check("prefs persist", st == 200 and json.loads(b)["prefs"].get("pref.theme") == "dark", str(st))
st, b = req("PATCH", "/api/v1/me/prefs", {"prefs": {"pref.theme": None}}, alice)
check("prefs null deletes key",
      st == 200 and "pref.theme" not in json.loads(b)["prefs"], str(st))
st, b = req("GET", "/api/v1/me/prefs", None, p0x)
check("prefs per-user isolation",
      st == 200 and "pref.mail.autoimg" not in json.loads(b)["prefs"], str(st))
# cleanup so reruns stay deterministic
req("PATCH", "/api/v1/me/prefs", {"prefs": {"pref.mail.autoimg": None, "pref.mail.trusted": None}}, alice)

# ---- HR: requests + approvals + attendance ----
st, b = req("POST", "/api/v1/hr/requests",
            {"kind": "leave", "payload": {"leave_type": "annual", "start_date": "2026-11-03",
                                          "end_date": "2026-11-04", "days": 2, "reason": "p0x"}}, alice)
leave_id = json.loads(b).get("id") if st == 200 else None
check("hr leave create", st == 200 and json.loads(b).get("status") == "pending", str(st))
st, b = req("POST", "/api/v1/hr/requests",
            {"kind": "expense", "payload": {"category": "travel", "amount_cents": 12500}}, alice)
exp_id = json.loads(b).get("id") if st == 200 else None
check("hr expense create", st == 200 and exp_id, str(st))
st, b = req("POST", "/api/v1/hr/requests",
            {"kind": "leave", "payload": {"leave_type": "annual"}}, alice)
check("hr bad payload rejected", st in (400, 422), str(st))
st, b = req("GET", "/api/v1/hr/requests?scope=pending", None, alice)
check("hr pending non-admin denied", st == 403, str(st))
st, b = req("GET", "/api/v1/hr/requests?scope=pending", None, admin)
pend = json.loads(b).get("requests", []) if st == 200 else []
check("hr admin pending queue",
      st == 200 and any(r["id"] == leave_id for r in pend) and any(r["id"] == exp_id for r in pend),
      str(len(pend)))
st, b = req("GET", "/api/v1/hr/requests?scope=mine", None, alice)
mine = json.loads(b).get("requests", []) if st == 200 else []
check("hr mine list", st == 200 and all(r["requester_name"] for r in mine), str(len(mine)))
st, b = req("POST", f"/api/v1/hr/requests/{leave_id}/decide",
            {"approve": True, "note": "ok"}, alice)
check("hr decide non-admin denied", st == 403, str(st))
st, b = req("POST", f"/api/v1/hr/requests/{leave_id}/decide",
            {"approve": True, "note": "ok"}, admin)
check("hr approve", st == 200 and json.loads(b).get("status") == "approved", str(st))
st, b = req("POST", f"/api/v1/hr/requests/{leave_id}/decide", {"approve": False}, admin)
check("hr re-decide conflict", st == 409, str(st))
st, b = req("POST", f"/api/v1/hr/requests/{exp_id}/cancel", None, admin)
check("hr cancel non-owner denied", st == 409, str(st))
st, b = req("POST", f"/api/v1/hr/requests/{exp_id}/cancel", None, alice)
check("hr owner cancel", st in (200, 204), str(st))
st, b = req("POST", f"/api/v1/hr/requests/{exp_id}/decide", {"approve": True}, admin)
check("hr cancelled not decidable", st == 409, str(st))

st, b = req("POST", "/api/v1/hr/attendance/check-out", None, p0x)
check("hr checkout without checkin", st == 409, str(st))
st, b = req("POST", "/api/v1/hr/attendance/check-in", {"note": "p0x"}, p0x)
check("hr check-in", st in (200, 204), str(st))
st, b = req("POST", "/api/v1/hr/attendance/check-out", None, p0x)
check("hr check-out", st in (200, 204), str(st))
mon = time.strftime("%Y-%m")
st, b = req("GET", f"/api/v1/hr/attendance/mine?month={mon}", None, p0x)
msum = json.loads(b).get("summary", {}) if st == 200 else {}
check("hr my month", st == 200 and msum.get("days") == 1 and msum.get("missing_checkout") == 0,
      str(msum))
st, b = req("GET", f"/api/v1/hr/attendance/summary?month={mon}", None, alice)
check("hr summary non-admin denied", st == 403, str(st))
st, b = req("GET", f"/api/v1/hr/attendance/summary?month={mon}", None, admin)
rows = json.loads(b).get("rows", []) if st == 200 else []
check("hr admin summary", st == 200 and any(r.get("days") == 1 for r in rows), str(len(rows)))
check("hr summary all staff rows", st == 200 and len(rows) >= 3, str(len(rows)))
check("hr summary leave_days field",
      st == 200 and all("leave_days" in r for r in rows), str(st))

# HR attachment ACL: requester + admin(approver) can read, stranger cannot.
st, obj = upload_purpose(alice, "hr", "receipt.txt", "text/plain", b"receipt-bytes")
hr_obj = obj.get("id") if st == 200 else None
check("hr attachment upload", st == 200 and hr_obj, str(st))
st, b = req("POST", "/api/v1/hr/requests",
            {"kind": "expense", "payload": {"category": "travel", "amount_cents": 8800,
                                            "attachment_object_ids": [hr_obj]}}, alice)
hr_exp = json.loads(b).get("id") if st == 200 else None
check("hr expense w/ attachment", st == 200 and hr_exp, str(st))
st, b = req("GET", f"/api/v1/objects/{hr_obj}/download", None, admin)
check("hr att readable by admin approver", st == 200 and b == b"receipt-bytes", str(st))
st, b = req("GET", f"/api/v1/objects/{hr_obj}/download", None, p0x)
check("hr att denied to stranger", st == 403, str(st))
st, b = req("GET", f"/api/v1/objects/{hr_obj}/download", None, alice)
check("hr att readable by requester", st == 200, str(st))
req("POST", f"/api/v1/hr/requests/{hr_exp}/cancel", None, alice)

st, b = req("GET", "/api/v1/hr/requests?scope=history", None, alice)
hist = json.loads(b).get("requests", []) if st == 200 else []
check("hr history member own only",
      st == 200 and all(r["requester_email"].endswith("alice@wittyloong.com") for r in hist)
      and all(r["status"] != "pending" for r in hist), str(len(hist)))
st, b = req("GET", "/api/v1/hr/requests?scope=history", None, admin)
check("hr history admin all",
      st == 200 and all(r["status"] != "pending" for r in json.loads(b)["requests"]), str(st))

# --- OnlyOffice: signed edit/file URLs, JWT-guarded callback, ACL ---
# Only present when WITTYCHAT_ONLYOFFICE_URL is configured on the server.
_noredir = urllib.request.build_opener(
    type("NoRedirect", (urllib.request.HTTPRedirectHandler,),
         {"redirect_request": lambda *a: None})())
def req_noredir(path, token=None):
    r = urllib.request.Request(BASE + path)
    if token:
        r.add_header("Authorization", f"Bearer {token}")
    try:
        resp = _noredir.open(r, timeout=10)
        return resp.status, resp.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()

st, obj = upload_purpose(alice, "attachment", "note.docx",
                         "application/octet-stream", b"pk-docx")
oo_obj = obj.get("id") if st == 200 else None
probe, _ = req_noredir(f"/office/open?obj={oo_obj}&token={alice}")
if probe == 404:
    print("SKIP office checks (WITTYCHAT_ONLYOFFICE_URL not set)")
else:
    check("office open 302->edit", probe == 302, str(probe))
    st, b = req("GET", f"/api/v1/objects/{oo_obj}/office-url", None, alice)
    edit_url = json.loads(b).get("url", "") if st == 200 else ""
    check("office-url api", st == 200 and "/office/edit?" in edit_url, str(st))
    st, b = req_noredir(edit_url.replace(BASE, "", 1))
    check("office edit page", st == 200 and b"DocEditor" in b and b"callbackUrl" in b, str(st))
    st, _ = req_noredir(f"/office/open?obj={oo_obj}&token={p0x}")
    check("office open denied to stranger", st in (401, 403), str(st))
    st, _ = req("GET", "/office/edit?obj=bad&u=bad&exp=1&sig=x")
    check("office edit bad params", st in (400, 403), str(st))
    st, _ = req("GET", f"/api/v1/office/file?obj={oo_obj}&exp=1&sig=bad")
    check("office file bad sig", st in (400, 403), str(st))
    st, _ = req("POST", f"/api/v1/office/callback?obj={oo_obj}", raw=b"{}")
    check("office callback needs JWT", st == 401, str(st))
    st, obj = upload_purpose(alice, "attachment", "photo.png", "image/png",
                             b"\x89PNG\r\n\x1a\n")
    png_obj = obj.get("id") if st == 200 else None
    st, _ = req_noredir(f"/office/open?obj={png_obj}&token={alice}")
    check("office open non-doc 400", st == 400, str(st))

# ================= collaborative docs / 协同文档 =================
st, b = req("POST", "/api/v1/docs", {"title": "p0x-doc"}, alice)
check("doc create", st == 200 and json.loads(b).get("id"), str(st))
doc_id = json.loads(b).get("id") if st == 200 else None
st, b = req("GET", "/api/v1/docs", None, alice)
check("doc list", st == 200 and any(d.get("id") == doc_id for d in json.loads(b).get("docs", [])))
st, b = req("POST", f"/api/v1/docs/{doc_id}/rename", {"title": "p0x-doc-2"}, alice)
check("doc rename", st == 204, str(st))
st, b = req("GET", f"/api/v1/docs/{doc_id}", None, p0x)
check("doc standalone denied to stranger", st == 403, str(st))
st, b = req("GET", f"/api/v1/docs/{doc_id}", None, alice)
check("doc get owner", st == 200 and json.loads(b).get("title") == "p0x-doc-2", str(st))

# conv-bound doc: visible to conv members (p0x), denied to outsiders (admin)
st, b = req("POST", "/api/v1/docs", {"title": "p0x-convdoc", "conv_id": conv}, alice)
check("conv doc create", st == 200, str(st))
cdoc_id = json.loads(b).get("id") if st == 200 else None
st, b = req("GET", f"/api/v1/docs/{cdoc_id}", None, p0x)
check("conv doc member access", st == 200, str(st))
st, b = req("GET", f"/api/v1/docs/{cdoc_id}", None, admin)
check("conv doc denied to outsider", st == 403, str(st))

# ---- doc kinds: sheet/slide accepted, bad kind rejected ----
st, b = req("POST", "/api/v1/docs", {"title": "p0x-sheet", "kind": "sheet"}, alice)
sheet_id = json.loads(b).get("id")
check("doc create sheet kind", st == 200 and json.loads(b).get("kind") == "sheet", str(st))
st, b = req("POST", "/api/v1/docs", {"title": "p0x-slide", "kind": "slide"}, alice)
slide_id = json.loads(b).get("id")
check("doc create slide kind", st == 200 and json.loads(b).get("kind") == "slide", str(st))
st, b = req("POST", "/api/v1/docs", {"title": "p0x-badkind", "kind": "pdf"}, alice)
check("doc bad kind rejected", st in (400, 403, 422), str(st))

# ---- conv docs panel listing: member sees bound docs, outsider 403 ----
st, b = req("GET", f"/api/v1/conversations/{conv}/docs", None, p0x)
check("conv docs list member", st == 200
      and any(d.get("id") == cdoc_id for d in json.loads(b).get("docs", [])), str(st))
st, b = req("GET", f"/api/v1/conversations/{conv}/docs", None, admin)
check("conv docs denied to outsider", st == 403, str(st))

# ---- doc delete: member-without-admin 403, owner 204, gone after ----
st, b = req("POST", "/api/v1/docs", {"title": "p0x-deldoc", "conv_id": conv}, alice)
del_id = json.loads(b).get("id")
st, b = req("DELETE", f"/api/v1/docs/{del_id}", None, p0x)
check("doc delete member denied", st == 403, str(st))
st, b = req("DELETE", f"/api/v1/docs/{del_id}", None, alice)
check("doc delete owner", st == 204, str(st))
st, b = req("GET", f"/api/v1/docs/{del_id}", None, alice)
check("doc gone after delete", st in (403, 404), str(st))
for _id, _nm in [(sheet_id, "sheet"), (slide_id, "slide")]:
    st, _b = req("DELETE", f"/api/v1/docs/{_id}", None, alice)
    check(f"doc delete {_nm}", st == 204, str(st))

# doc WS relay: handshake frames + update persist + late-join replay
try:
    import websocket as _wsm
    def _vu(n):
        out = bytearray()
        while n >= 0x80:
            out.append((n & 0x7F) | 0x80); n >>= 7
        out.append(n); return bytes(out)
    def _vb(d):
        return _vu(len(d)) + d
    wsa = _wsm.create_connection(
        f"ws://localhost:8080/ws/doc/{cdoc_id}?token={alice}", timeout=5)
    f1 = wsa.recv()
    check("doc ws handshake frame", isinstance(f1, (bytes, str)) and len(f1) > 0, type(f1).__name__)
    # sync update frame: msgType=0, sub=2, payload=fake update blob
    wsa.send(_vu(0) + _vu(2) + _vb(b"p0x-fake-upd"), _wsm.ABNF.OPCODE_BINARY)
    wsa.close()
    time.sleep(0.3)
    wsb = _wsm.create_connection(
        f"ws://localhost:8080/ws/doc/{cdoc_id}?token={p0x}", timeout=5)
    wsb.settimeout(2)
    got = b""
    for _ in range(10):
        try:
            got += wsb.recv()
        except Exception:
            break
        if b"p0x-fake-upd" in got:
            break
    check("doc ws update persisted+replayed", b"p0x-fake-upd" in got, str(len(got)))
    wsb.close()
    try:
        _wsm.create_connection(
            f"ws://localhost:8080/ws/doc/{cdoc_id}?token={admin}", timeout=5)
        check("doc ws denied to outsider", False, "connected")
    except Exception as e:
        check("doc ws denied to outsider", "403" in str(e) or "forbidden" in str(e).lower(), str(e)[:60])
    try:
        _wsm.create_connection(
            f"ws://localhost:8080/ws/doc/{cdoc_id}?token=bad", timeout=5)
        check("doc ws bad token", False, "connected")
    except Exception as e:
        check("doc ws bad token", "401" in str(e) or "unauthorized" in str(e).lower(), str(e)[:60])
except ImportError:
    print("SKIP doc ws checks (websocket-client not installed)")

print(f"\n{len(passed)} passed, {len(failed)} failed")
sys.exit(1 if failed else 0)
