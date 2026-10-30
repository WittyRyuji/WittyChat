#!/usr/bin/env python3
"""P0 end-to-end smoke: activation -> messaging -> attachment -> mail -> admin ops.
Usage: python scripts/smoke_p0.py  (server on :8080, worker running)
"""
import json, sys, urllib.request, urllib.error, uuid, time

BASE = "http://localhost:8080"
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

# --- auth ---
st, b = req("POST", "/api/v1/auth/login", {"email": "admin@wittyloong.com", "password": "DevPass123!",
                                           "device_name": "smoke", "device_kind": "web"})
check("admin login", st == 200, str(st))
admin = json.loads(b)["session_token"] if st == 200 else None
if not admin:
    sys.exit(1)

st, b = req("POST", "/api/v1/auth/login", {"email": "alice@wittyloong.com", "password": "AlicePass123!",
                                           "device_name": "smoke", "device_kind": "web"})
alice = json.loads(b)["session_token"] if st == 200 else None
check("alice login", alice is not None, str(st))

# --- new user provisioning (activation link -> set password -> login) ---
email = f"smoke-{uuid.uuid4().hex[:8]}@wittyloong.com"
st, b = req("POST", "/api/v1/admin/users", {"email": email, "display_name": "Smoke"}, admin)
check("admin create user", st in (200, 201), str(st))
body = json.loads(b)
act_url = body["activation_url"]
uid = body["user"]["id"]
from urllib.parse import urlparse, parse_qs
tok = parse_qs(urlparse(act_url).query).get("token", [act_url.rsplit("/", 1)[-1]])[0]
st, b = req("POST", "/api/v1/auth/activate", {"token": tok, "password": "SmokePass123!"})
check("activate link", st == 200, str(st))
st, b = req("POST", "/api/v1/auth/login", {"email": email, "password": "SmokePass123!",
                                           "device_name": "s", "device_kind": "web"})
smoke = json.loads(b)["session_token"] if st == 200 else None
check("new user login", smoke is not None, str(st))

# --- IM: dm + markdown + read ---
st, b = req("POST", "/api/v1/conversations/dm", {"user_id": json.loads(
    req("GET", "/api/v1/auth/me", None, smoke)[1])["user"]["id"]}, alice)
conv = (json.loads(b)["id"] if st in (200, 201) else None) or (json.loads(b).get("conversation", {}) or {}).get("id")
check("alice->smoke dm", conv is not None, str(st))
mid = str(uuid.uuid4())
st, b = req("POST", f"/api/v1/conversations/{conv}/messages",
            {"client_msg_id": mid, "markdown": "**smoke** `md`"}, alice)
check("send message", st == 201, str(st))
seq = json.loads(b)["op_seq"] if st == 201 else 0
st, b = req("GET", f"/api/v1/conversations/{conv}/ops?after=0", None, smoke)
ops = json.loads(b)["ops"] if st == 200 else []
check("smoke sees message", any(o["op_seq"] == seq for o in ops), str(st))
st, b = req("POST", f"/api/v1/conversations/{conv}/read", {"op_seq": seq}, smoke)
check("mark read", st == 204, str(st))
st, b = req("GET", "/api/v1/conversations", None, alice)
c0 = [c for c in json.loads(b)["conversations"] if c["id"] == conv][0]
check("alice sees peer read", c0.get("peer_read_seq", 0) >= seq, json.dumps(c0.get("peer_read_seq")))

# --- attachment upload + download + quota endpoint ---
raw = b"smoke-file-" + uuid.uuid4().bytes
boundary = "X" + uuid.uuid4().hex
multipart = (f"--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"smoke.txt\"\r\n"
             f"Content-Type: text/plain\r\n\r\n").encode() + raw + f"\r\n--{boundary}--\r\n".encode()
st, b = req("POST", f"/api/v1/conversations/{conv}/attachments", None, alice,
            raw=multipart, ctype=f"multipart/form-data; boundary={boundary}")
obj = json.loads(b) if st == 200 else {}
check("attachment upload", st == 200 and obj.get("id"), str(st))
if obj.get("id"):
    st, b = req("GET", f"/api/v1/objects/{obj['id']}/download", None, smoke)
    check("peer download", st == 200 and b == raw, str(st))
    st, b = req("GET", f"/api/v1/objects/{obj['id']}/url", None, smoke)
    sig = json.loads(b)["url"] if st == 200 else ""
    st2, b2 = req("GET", sig, None)  # signed url, no auth header
    check("signed url download", st2 == 200 and b2 == raw, f"{st2}")
st, b = req("GET", "/api/v1/objects/quota", None, alice)
check("quota endpoint", st == 200 and "used_bytes" in b.decode(), str(st))

# --- mail: internal delivery alice -> smoke ---
st, b = req("POST", "/api/v1/mail/send", {"to": [email], "subject": "P0 smoke",
                                          "body": "hello from smoke"}, alice)
check("mail send", st == 201, str(st))
time.sleep(0.3)
st, b = req("GET", "/api/v1/mail/folders", None, smoke)
folders = {f["kind"]: f for f in json.loads(b)} if st == 200 else {}
check("mail folders exist", "inbox" in folders, str(st))
inbox = folders.get("inbox", {}).get("id")
st, b = req("GET", f"/api/v1/mail/messages?folder={inbox}", None, smoke)
msgs = json.loads(b) if st == 200 else []
hit = [m for m in msgs if m["subject"] == "P0 smoke"]
check("mail delivered internally", len(hit) == 1, str(len(msgs)))
if hit:
    st, b = req("GET", f"/api/v1/mail/messages/{hit[0]['id']}", None, smoke)
    m = json.loads(b) if st == 200 else {}
    check("mail body + read flag", m.get("text_body") == "hello from smoke", str(st))
    st, b = req("GET", "/api/v1/mail/search?q=smoke", None, smoke)
    check("mail search", st == 200 and len(json.loads(b)) >= 1, str(st))

# --- admin: audit export + settings ---
st, b = req("GET", "/api/v1/admin/audit/export", None, admin)
check("audit csv export", st == 200 and b.startswith(b"id,"), str(st))
st, b = req("POST", f"/api/v1/admin/users/{uid}/deactivate", None, admin)
check("admin deactivate", st in (200, 204), str(st))
st, b = req("GET", f"/api/v1/conversations", None, smoke)
check("deactivated user blocked", st == 401, str(st))

print(f"\n{len(passed)} passed, {len(failed)} failed")
sys.exit(1 if failed else 0)
