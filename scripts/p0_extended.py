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
st, b = req("GET", f"/api/v1/objects/{img1}/thumb?token={p0x}", None)
check("peer reads thumb", st == 200 and b[:2] == b"\xff\xd8", str(st))
st, b = req("GET", f"/api/v1/objects/{img1}/thumb", None, admin)
check("non-member thumb denied", st == 403, str(st))

# date-partitioned storage on disk
today = time.strftime("%Y/%m/%d")
obj_dir = os.path.join(DATA, "objects", *today.split("/"))
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

print(f"\n{len(passed)} passed, {len(failed)} failed")
sys.exit(1 if failed else 0)
