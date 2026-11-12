#!/usr/bin/env python3
"""P0 perf spot-check: 10k-msg conversation fetch, 1k-op delta, 1000-conv directory.
Seeds via SQL (COPY-ish bulk insert) then times API calls.
"""
import json, os, sys, time, urllib.request, uuid, subprocess

BASE = "http://localhost:8080"
DOCKER = r"C:/Program Files/Docker/Docker/resources/bin/docker.exe"
PG_USER = os.environ.get("DEV_POSTGRES_USER", "wittychat")
PG_DB = os.environ.get("DEV_POSTGRES_DB", "wittychat")
PG_PASSWORD = os.environ.get("DEV_POSTGRES_PASSWORD", "")

def req(path, token):
    r = urllib.request.Request(BASE + path)
    r.add_header("Authorization", f"Bearer {token}")
    return json.loads(urllib.request.urlopen(r, timeout=120).read())

def login(email, pw):
    r = urllib.request.Request(BASE + "/api/v1/auth/login",
        data=json.dumps({"email": email, "password": pw, "device_name": "perf", "device_kind": "web"}).encode())
    r.add_header("Content-Type", "application/json")
    return json.loads(urllib.request.urlopen(r).read())

def psql(*args):
    return subprocess.run([DOCKER, "exec", "-e", f"PGPASSWORD={PG_PASSWORD}", "deploy-postgres-1",
        "psql", "-U", PG_USER, "-At", PG_DB, *args], check=True, capture_output=True, text=True)

def sql(q):
    psql("-c", q)

alice_email = os.environ.get("WITTYCHAT_DEV_ALICE_EMAIL", "alice@wittyloong.com")
alice_password = os.environ.get("WITTYCHAT_DEV_ALICE_PASSWORD", "alicepass")
login_body = login(alice_email, alice_password)
tok = login_body["session_token"]
me = login_body["user"]["id"]

# --- seed: conv + 10k message ops ---
print("seeding 10k messages…")
cid = psql("-c",
    f"INSERT INTO conversations (id,kind,title,owner_id) VALUES (gen_random_uuid(),'group','perf-10k','{me}') RETURNING id"
).stdout.strip().split()[0]
sql(f"INSERT INTO conv_members (conv_id,user_id,role) VALUES ('{cid}','{me}','owner') ON CONFLICT DO NOTHING")
t0 = time.time()
# bulk insert 10k ops via generate_series
sql(f"""INSERT INTO conv_ops (op_id,conv_id,op_seq,kind,actor_id,body,payload,created_at)
SELECT gen_random_uuid(), '{cid}', g, 'message', '{me}', 'perf msg '||g,
       jsonb_build_object('markdown','perf msg '||g), now()
FROM generate_series(1,10000) g""")
sql(f"UPDATE conversations SET last_op_seq=10000 WHERE id='{cid}'")
print(f"  inserted in {time.time()-t0:.1f}s")

# --- 1) full fetch limit 200 (page) ---
t0 = time.time()
ops = req(f"/api/v1/conversations/{cid}/ops?after=0&limit=200", tok)
dt = time.time() - t0
print(f"first page (200 of 10k): {dt*1000:.0f}ms  ops={len(ops['ops'])}  latest={ops['latest_seq']}")

# --- 2) delta after=9000 (1k ops) ---
t0 = time.time()
t0 = time.time()
total = 0
after = 9000
while True:
    page = req(f"/api/v1/conversations/{cid}/ops?after={after}&limit=500", tok)
    total += len(page["ops"])
    if not page["ops"] or len(page["ops"]) < 500:
        break
    after = page["ops"][-1]["op_seq"]
dt = time.time() - t0
print(f"delta after=9000 (~1k ops, paged): {dt*1000:.0f}ms  ops={total}")

# --- 3) before-page (scrollback) ---
t0 = time.time()
ops = req(f"/api/v1/conversations/{cid}/ops?before=5000&limit=50", tok)
dt = time.time() - t0
print(f"before=5000 page: {dt*1000:.0f}ms  ops={len(ops['ops'])}")

# --- 4) 1000-conversation directory ---
print("seeding 1000 conversations…")
sql(f"""INSERT INTO conversations (id,kind,title,owner_id,last_op_seq)
SELECT gen_random_uuid(),'dm','perf-'||g,'{me}',g FROM generate_series(1,1000) g""")
sql(f"""INSERT INTO conv_members (conv_id,user_id,role,last_read_op_seq)
SELECT c.id,'{me}','member',c.last_op_seq-3 FROM conversations c
WHERE c.title LIKE 'perf-%' AND NOT EXISTS
 (SELECT 1 FROM conv_members m WHERE m.conv_id=c.id AND m.user_id='{me}')""")
t0 = time.time()
d = req("/api/v1/conversations", tok)
dt = time.time() - t0
print(f"directory ({len(d['conversations'])} convs): {dt*1000:.0f}ms")

# --- cleanup ---
sql(f"DELETE FROM conv_members WHERE conv_id IN (SELECT id FROM conversations WHERE title LIKE 'perf-%')")
sql(f"DELETE FROM conv_ops WHERE conv_id='{cid}'")
sql(f"DELETE FROM conversations WHERE title LIKE 'perf-%'")
print("cleaned up")
