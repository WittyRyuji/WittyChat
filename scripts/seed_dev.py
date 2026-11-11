#!/usr/bin/env python3
"""Dev-environment seeder for WittyChat.

Assumes a freshly reset database with the bootstrap admin
(admin@wittyloong.com / DevPass123!). Creates Alice/Carol itself —
an existing user that logs in with the expected password is reused, a
still-pending one is activated via an admin-issued activation link.
Content (messages, stickers, mail) is NOT deduplicated: run it once per
reset.

Deps: Python 3.10+ stdlib (urllib only) + Pillow (fixture generation).
The server must be running on :8080 (worker too, for thumb tasks).
Fixtures are generated into a temp dir, never the repo. The manifest
lands at %TEMP%\\wc_seed\\manifest.json (or $TMPDIR on other OSes).

Usage: PYTHONIOENCODING=utf-8 python scripts/seed_dev.py
"""
import hashlib
import io
import json
import os
import sys
import tempfile
import time
import urllib.error
import urllib.request
import uuid
from urllib.parse import parse_qs, urlparse

from PIL import Image, ImageDraw, ImageFont, ImageOps

BASE = os.environ.get("WITTYCHAT_BASE", "http://localhost:8080")
OUT_DIR = os.path.join(tempfile.gettempdir(), "wc_seed")
FIX_DIR = os.path.join(OUT_DIR, "fixtures")
MANIFEST = os.path.join(OUT_DIR, "manifest.json")

ADMIN = ("admin@wittyloong.com", "DevPass123!")
# The user's known dev passwords are shorter than the NIST 15-char
# default, so the script first relaxes security.min_password to the
# validation floor of 8 — a DEV-ONLY policy relaxation via the real
# admin settings API (do not ship this against production).
USERS = [
    ("alice@wittyloong.com", "Alice", "AlicePass123!"),
    ("carol@wittyloong.com", "Carol", "CarolPass123"),
]


def req(method, path, body=None, token=None, raw=None, ctype="application/json"):
    data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
    r = urllib.request.Request(BASE + path, data=data, method=method)
    if data is not None:
        r.add_header("Content-Type", ctype)
    if token:
        r.add_header("Authorization", f"Bearer {token}")
    try:
        resp = urllib.request.urlopen(r, timeout=30)
        return resp.status, resp.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def must(name, cond, detail=""):
    if not cond:
        raise SystemExit(f"FAIL {name}: {detail}")
    print("ok", name)


def login(email, password):
    st, b = req("POST", "/api/v1/auth/login",
                {"email": email, "password": password,
                 "device_name": "seed", "device_kind": "web"})
    if st == 200:
        return json.loads(b)["session_token"]
    return None


def me_id(token):
    st, b = req("GET", "/api/v1/auth/me", None, token)
    must("auth/me", st == 200, st)
    return json.loads(b)["user"]["id"]


def ensure_user(admin_tok, email, name, password):
    """Idempotent user provisioning via the real admin flow."""
    tok = login(email, password)
    if tok:
        return tok, me_id(tok)
    st, b = req("POST", "/api/v1/admin/users",
                {"email": email, "display_name": name}, admin_tok)
    if st not in (200, 201):
        # Already exists (fresh run died mid-way, or reseed over a used
        # DB) → admin-issued ACTIVATION link, not reset: only the
        # activation kind flips a pending user to active, and it is a
        # no-op on an already-active one.
        st2, b2 = req("GET", "/api/v1/admin/users", None, admin_tok)
        must("admin list users", st2 == 200, st2)
        users = json.loads(b2)
        users = users["users"] if isinstance(users, dict) else users
        uid = next((u["id"] for u in users if u["email"] == email), None)
        must(f"find existing user {email}", uid is not None)
        st, b = req("POST", f"/api/v1/admin/users/{uid}/links",
                    {"kind": "activation"}, admin_tok)
        must(f"issue link {email}", st in (200, 201), f"{st} {b[:200]}")
    body = json.loads(b)
    act_url = body.get("activation_url") or body.get("url")
    must(f"activation url {email}", act_url is not None, str(body)[:200])
    tok = parse_qs(urlparse(act_url).query).get("token", [act_url.rsplit("/", 1)[-1]])[0]
    st, b = req("POST", "/api/v1/auth/activate", {"token": tok, "password": password})
    must(f"activate {email}", st == 200, f"{st} {b[:200]}")
    # The activate endpoint auto-logs-in — use its session directly.
    tok2 = json.loads(b)["session_token"]
    return tok2, me_id(tok2)


def upload(token, path, filename, blob, ctype):
    boundary = "X" + uuid.uuid4().hex
    body = (f"--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; "
            f"filename=\"{filename}\"\r\nContent-Type: {ctype}\r\n\r\n").encode()
    body += blob + f"\r\n--{boundary}--\r\n".encode()
    st, b = req("POST", path, None, token,
                raw=body, ctype=f"multipart/form-data; boundary={boundary}")
    if st != 200:
        raise SystemExit(f"upload {filename} failed: {st} {b[:300]}")
    return json.loads(b)


def send(token, conv, markdown="", object_ids=None, quote_op_seq=None,
         mentions=None, mention_all=False):
    body = {"client_msg_id": str(uuid.uuid4()), "markdown": markdown}
    if quote_op_seq:
        body["quote_op_seq"] = quote_op_seq
    if mentions:
        body["mentions"] = mentions
    if mention_all:
        body["mention_all"] = True
    if object_ids:
        body["object_ids"] = object_ids
    st, b = req("POST", f"/api/v1/conversations/{conv}/messages", body, token)
    if st not in (200, 201):
        raise SystemExit(f"send failed: {st} {b[:300]}")
    return json.loads(b)


def react(token, conv, seq, emoji, add=True):
    st, b = req("POST", f"/api/v1/conversations/{conv}/messages/{seq}/reaction",
                {"emoji": emoji, "add": add}, token)
    must(f"react {emoji} on {seq}", st == 200, f"{st} {b[:200]}")


def mark_read(token, conv, seq):
    st, _ = req("POST", f"/api/v1/conversations/{conv}/read", {"op_seq": seq}, token)
    must(f"read {conv[:8]} seq {seq}", st == 204, st)


def latest_seq(token, conv):
    st, b = req("GET", f"/api/v1/conversations/{conv}/ops?after=0&limit=1", None, token)
    must("ops latest", st == 200, st)
    return json.loads(b)["latest_seq"]


# ---------------- fixtures ----------------

CT = {"png": "image/png", "jpg": "image/jpeg", "gif": "image/gif",
      "webp": "image/webp", "bmp": "image/bmp", "svg": "image/svg+xml",
      "pdf": "application/pdf", "txt": "text/plain", "csv": "text/csv"}

RED, BLUE = (220, 30, 30), (30, 60, 220)
BG = (240, 238, 234)


def font(px):
    try:
        return ImageFont.load_default(size=px)
    except TypeError:
        return ImageFont.load_default()


def draw_display(w, h):
    """The INTENDED display image: red block top-left, blue block
    bottom-right, 'TOP' text near the top."""
    im = Image.new("RGB", (w, h), BG)
    d = ImageDraw.Draw(im)
    bs = max(24, min(w, h) // 5)
    d.rectangle([6, 6, 6 + bs, 6 + bs], fill=RED)
    d.rectangle([w - 6 - bs, h - 6 - bs, w - 6, h - 6], fill=BLUE)
    t = d.textbbox((0, 0), "TOP", font=font(max(14, bs // 3)))
    d.text(((w - (t[2] - t[0])) / 2, 8), "TOP", fill=(20, 20, 20),
           font=font(max(14, bs // 3)))
    return im


def exif_jpeg(display, orientation):
    """Derive the raw stored pixels (inverse display transform) and tag
    the EXIF orientation — the file decodes sideways but must be shown
    upright. / 由目标显示图反推出原始存储像素并写入方向标记。"""
    raw = {6: display.rotate(90, expand=True),    # stored = display rot 90 CCW
           8: display.rotate(-90, expand=True),   # stored = display rot 90 CW
           3: display.rotate(180)}[orientation]
    buf = io.BytesIO()
    ex = Image.Exif()
    ex[0x0112] = orientation
    raw.convert("RGB").save(buf, "JPEG", quality=92, exif=ex)
    blob = buf.getvalue()
    # Self-check: a correct viewer (exif_transpose) restores the display —
    # the red block must be back in the top-left.
    fixed = ImageOps.exif_transpose(Image.open(io.BytesIO(blob)))
    r, g, b = fixed.getpixel((12, 12))
    if not (r > 150 and g < 100 and b < 100):
        raise SystemExit(f"exif o{orientation} self-check failed: pixel {r},{g},{b}")
    return blob


def anim_gif(w, h):
    frames = []
    for i in range(3):
        im = draw_display(w, h)
        d = ImageDraw.Draw(im)
        d.ellipse([w // 2 - 30 + i * 20, h // 2 - 20, w // 2 + 30 + i * 20, h // 2 + 20],
                  fill=(40 + i * 60, 180, 60))
        frames.append(im)
    buf = io.BytesIO()
    frames[0].save(buf, "GIF", save_all=True, append_images=frames[1:],
                   duration=250, loop=0)
    return buf.getvalue()


def minimal_pdf():
    stream = b"BT /F1 12 Tf 72 720 Td (WittyChat seed report) Tj ET"
    objs = [
        b"<</Type/Catalog/Pages 2 0 R>>",
        b"<</Type/Pages/Kids[3 0 R]/Count 1>>",
        b"<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]"
        b"/Contents 4 0 R/Resources<</Font<</F1 5 0 R>>>>>>",
        b"<</Length %d>>stream\n" % len(stream) + stream + b"\nendstream",
        b"<</Type/Font/Subtype/Type1/BaseFont/Helvetica>>",
    ]
    out = io.BytesIO()
    out.write(b"%PDF-1.4\n")
    offs = []
    for i, o in enumerate(objs, 1):
        offs.append(out.tell())
        out.write(b"%d 0 obj\n" % i + o + b"\nendobj\n")
    xref = out.tell()
    out.write(b"xref\n0 %d\n" % (len(objs) + 1))
    out.write(b"0000000000 65535 f \n")
    for off in offs:
        out.write(b"%010d 00000 n \n" % off)
    out.write(b"trailer\n<</Size %d/Root 1 0 R>>\nstartxref\n%d\n%%%%EOF\n"
              % (len(objs) + 1, xref))
    return out.getvalue()


def png_blob(w, h, marked=True):
    im = draw_display(w, h) if marked else Image.new("RGB", (w, h), BG)
    buf = io.BytesIO()
    im.save(buf, "PNG")
    return buf.getvalue()


def webp_blob(w, h):
    buf = io.BytesIO()
    draw_display(w, h).save(buf, "WEBP", quality=90)
    return buf.getvalue()


def bmp_blob(w, h):
    buf = io.BytesIO()
    draw_display(w, h).save(buf, "BMP")
    return buf.getvalue()


def corrupt_png():
    return b"\x89PNG\r\n\x1a\n" + b"\x99" * 256


FIXTURES = [
    # (name, ctype, blob-fn, expected display w, h)
    ("land517.png", CT["png"], lambda: png_blob(517, 389), 517, 389),
    ("port300x480.png", CT["png"], lambda: png_blob(300, 480), 300, 480),
    ("exif6.jpg", CT["jpg"], lambda: exif_jpeg(draw_display(300, 400), 6), 300, 400),
    ("exif8.jpg", CT["jpg"], lambda: exif_jpeg(draw_display(300, 400), 8), 300, 400),
    ("exif3.jpg", CT["jpg"], lambda: exif_jpeg(draw_display(400, 300), 3), 400, 300),
    ("pano.png", CT["png"], lambda: png_blob(2400, 300), 2400, 300),
    ("tall.png", CT["png"], lambda: png_blob(300, 1600), 300, 1600),
    ("tiny32.png", CT["png"], lambda: png_blob(32, 32), 32, 32),
    ("px1.png", CT["png"], lambda: png_blob(1, 1, marked=False), 1, 1),
    ("anim.gif", CT["gif"], lambda: anim_gif(480, 360), 480, 360),
    ("pic.webp", CT["webp"], lambda: webp_blob(640, 480), 640, 480),
    ("pic.bmp", CT["bmp"], lambda: bmp_blob(200, 150), 200, 150),     # x/image/bmp decodes it
    ("pic.svg", CT["svg"], lambda: SVG_XML.encode(), None, None),     # Go can't decode; WebView2 can
    ("corrupt.png", CT["png"], corrupt_png, None, None),             # undecodable by design
]

# Vector fixture for the "Go can't decode, browser can" path — explicit
# width/height so the browser lays it out deterministically.
SVG_XML = (
    '<svg xmlns="http://www.w3.org/2000/svg" width="240" height="120" '
    'viewBox="0 0 240 120">'
    '<rect width="240" height="120" fill="#f0eee8"/>'
    '<rect x="6" y="6" width="48" height="48" fill="#dc1e1e"/>'
    '<rect x="186" y="66" width="48" height="48" fill="#1e3cdc"/>'
    '<text x="96" y="30" font-size="20" fill="#141414">TOP</text>'
    '</svg>'
)

FILES = [
    ("report.pdf", CT["pdf"], minimal_pdf),
    ("notes.txt", CT["txt"], lambda: "种子备注：第一行\nsecond line\n" * 8),
    ("data.csv", CT["csv"], lambda: "name,qty,price\nwidget,3,9.99\ngadget,1,42.00\n"),
]


def main():
    os.makedirs(FIX_DIR, exist_ok=True)
    blobs = {}
    manifest = {"base": BASE, "users": {}, "conversations": {}, "objects": {}, "stickers": []}

    # materialize fixtures (exif self-checks happen inside exif_jpeg)
    for name, ctype, fn, ew, eh in FIXTURES:
        blob = fn()
        open(os.path.join(FIX_DIR, name), "wb").write(blob)
        blobs[name] = blob
    for name, ctype, fn in FILES:
        data = fn()
        blob = data.encode() if isinstance(data, str) else data
        open(os.path.join(FIX_DIR, name), "wb").write(blob)
        blobs[name] = blob
    print(f"fixtures -> {FIX_DIR} ({len(blobs)} files)")

    admin_tok = login(*ADMIN)
    must("admin login", admin_tok is not None)
    # Dev-only: relax the password floor so the canonical dev passwords
    # pass the activation check (see USERS comment).
    st, b = req("PUT", "/api/v1/admin/settings",
                {"settings": {"security.min_password": 8}}, admin_tok)
    must("relax min_password", st in (200, 204), f"{st} {b[:200]}")
    toks = {"ryuji": admin_tok}
    ids = {"ryuji": me_id(admin_tok)}
    manifest["users"]["ryuji"] = {"id": ids["ryuji"], "email": ADMIN[0], "token": admin_tok}
    for email, name, pw in USERS:
        tok, uid = ensure_user(admin_tok, email, name, pw)
        must(f"user {name}", tok is not None)
        key = name.lower()
        toks[key], ids[key] = tok, uid
        manifest["users"][key] = {"id": uid, "email": email, "token": tok}
    print("users:", {k: v["id"] for k, v in manifest["users"].items()})

    # ---------- DM Ryuji <-> Alice ----------
    st, b = req("POST", "/api/v1/conversations/dm", {"user_id": ids["alice"]}, admin_tok)
    must("create dm", st in (200, 201), f"{st} {b[:200]}")
    dm = json.loads(b).get("id") or json.loads(b).get("conversation", {}).get("id")
    manifest["conversations"]["dm"] = dm
    print("dm:", dm)

    obj_ids = {}
    EXPECT = {f[0]: (f[3], f[4]) for f in FIXTURES}

    def dm_upload(who, fname):
        ext = fname.rsplit(".", 1)[-1].lower()
        o = upload(toks[who], f"/api/v1/conversations/{dm}/attachments",
                   fname, blobs[fname], CT[ext])
        ew, eh = EXPECT.get(fname, (None, None))
        manifest["objects"].setdefault(fname, {
            "id": o["id"], "sha256": hashlib.sha256(blobs[fname]).hexdigest(),
            "w": ew, "h": eh})
        return o["id"]

    senders = ["ryuji", "alice"]
    # every image fixture as its own solo image message (ctx attstk)
    for i, (name, *_rest) in enumerate(FIXTURES):
        who = senders[i % 2]
        oid = dm_upload(who, name)
        obj_ids[name] = oid
        ev = send(toks[who], dm, "", [oid])
        print(f"  solo {name:16s} by {who:5s} seq={ev['op_seq']}")
        if name == "anim.gif":
            react(toks["alice"], dm, ev["op_seq"], "👍")

    # one message with 3 images + text (ctx att), one image + caption
    trio = [dm_upload("alice", n) for n in ("tiny32.png", "px1.png", "pic.webp")]
    send(toks["alice"], dm, "三图连发，ctx=att 网格", trio)
    cap = dm_upload("ryuji", "land517.png")
    send(toks["ryuji"], dm, "带说明文字的图片", [cap])

    # text messages
    hi = send(toks["ryuji"], dm, "hi")
    send(toks["alice"], dm, "好")
    send(toks["ryuji"], dm, "🤣")
    para = send(toks["alice"], dm,
                ("这是一段很长的正文，用来验证聊天气泡在长文本下的换行与宽度。"
                 "It repeats enough English to mix scripts and stress the markdown "
                 "renderer across several sentences. ") * 3)
    send(toks["ryuji"], dm, "```\n" + "x" * 300 + "\n```")
    send(toks["alice"], dm,
         "https://wittyloong.com/some/very/long/path" + "/seg" * 30 +
         "?q=" + "a" * 120)
    send(toks["ryuji"], dm, "| 列A | 列B |\n|---|---|\n| 1 | 2 |\n| x | y |")
    send(toks["ryuji"], dm, "@Alice 看一下这个", mentions=[ids["alice"]])
    send(toks["alice"], dm, "引用回复", quote_op_seq=hi["op_seq"])
    doomed = send(toks["ryuji"], dm, "这条马上撤回")
    st, b = req("POST", f"/api/v1/conversations/{dm}/messages/{doomed['op_seq']}/recall",
                None, admin_tok)
    must("recall", st == 200, f"{st} {b[:200]}")

    # reactions: 👍 both on "hi"; 5 distinct on the long paragraph
    react(admin_tok, dm, hi["op_seq"], "👍")
    react(toks["alice"], dm, hi["op_seq"], "👍")
    for who, emo in (("ryuji", "👍"), ("ryuji", "😂"), ("ryuji", "🚀"),
                     ("alice", "🎉"), ("alice", "❤️")):
        react(toks[who], dm, para["op_seq"], emo)

    # file attachments
    for name, ctype, _ in FILES:
        oid = dm_upload("ryuji" if name != "data.csv" else "alice", name)
        send(toks["ryuji" if name != "data.csv" else "alice"], dm,
             f"附件 {name}", [oid])
    mark_read(admin_tok, dm, latest_seq(admin_tok, dm))
    mark_read(toks["alice"], dm, latest_seq(toks["alice"], dm))
    print("dm seeded")

    # ---------- group 测试群 ----------
    st, b = req("POST", "/api/v1/conversations/group",
                {"title": "测试群", "is_public": False,
                 "member_ids": [ids["alice"], ids["carol"]]}, admin_tok)
    must("create group", st in (200, 201), f"{st} {b[:200]}")
    grp = json.loads(b).get("id") or json.loads(b).get("conversation", {}).get("id")
    manifest["conversations"]["group"] = grp
    gmsgs = [
        ("alice", "大家好，测试群开了"),
        ("ryuji", "欢迎 👋"),
        ("carol", "收到，报道"),
        ("alice", "今天验证图片、邮件和文档"),
        ("ryuji", "@所有人 10 点同步进度"),
        ("carol", "好的"),
        ("alice", "长一点的消息凑数，看看群聊气泡排版是否正常。"),
        ("ryuji", "再来一条"),
        ("carol", "emoji 🎉🎊"),
    ]
    for who, text in gmsgs:
        body = {"mention_all": True} if text.startswith("@所有人") else {}
        send(toks[who], grp, text, mention_all=body.get("mention_all", False))
    # one group image
    gimg = upload(toks["carol"], f"/api/v1/conversations/{grp}/attachments",
                  "tiny32.png", blobs["tiny32.png"], CT["png"])
    send(toks["carol"], grp, "群里的图", [gimg["id"]])
    manifest["objects"]["group:tiny32.png"] = {
        "id": gimg["id"], "sha256": hashlib.sha256(blobs["tiny32.png"]).hexdigest(),
        "w": 32, "h": 32}
    last = latest_seq(admin_tok, grp)
    for who in ("ryuji", "alice", "carol"):
        mark_read(toks[who], grp, last)
    print("group seeded:", grp)

    # ---------- Ryuji's sticker shelf (server side objects) ----------
    # NOTE: the picker shelf itself is per-device localStorage
    # ("wittychat.stickers") — the seed can only create the server-side
    # objects; the manifest records the exact shelf JSON for phase 3.
    sticker_specs = (
        [(96, 96), (96, 96), (120, 120), (80, 80),
         (128, 96), (96, 128), (160, 120), (120, 160),
         (200, 80), (80, 200), (150, 150), (180, 60),
         (60, 180), (140, 100), (100, 140), (90, 90),
         (110, 130), (130, 110), (70, 70), (88, 132)]
        + [None, None]  # two GIF slots appended below
        + [(104, 96), (96, 104)]
    )
    sent_stickers = []
    for i, spec in enumerate(sticker_specs):
        if spec is None:
            w, h = 200, 160
            fname, blob, mime = f"stk{i:02d}.gif", anim_gif(w, h), CT["gif"]
        else:
            w, h = spec
            fname, blob, mime = f"stk{i:02d}.png", png_blob(w, h), CT["png"]
        o = upload(admin_tok, "/api/v1/objects/upload?purpose=sticker", fname, blob, mime)
        manifest["stickers"].append({"id": o["id"], "name": fname,
                                     "mime": o.get("mime") or mime})
        manifest["objects"][f"sticker:{fname}"] = {
            "id": o["id"], "sha256": hashlib.sha256(blob).hexdigest(),
            "w": w, "h": h}
        sent_stickers.append(o["id"])
    # send 2 stickers into the DM (same shape as the picker's sendSticker)
    send(toks["ryuji"], dm, "", [sent_stickers[3]])
    send(toks["ryuji"], dm, "", [sent_stickers[20]])
    print(f"stickers: {len(sent_stickers)} objects, 2 sent to dm")

    # ---------- mail ----------
    def mail_upload(who, fname):
        ext = fname.rsplit(".", 1)[-1].lower()
        o = upload(toks[who], "/api/v1/objects/upload?purpose=mail",
                   fname, blobs[fname], CT[ext])
        manifest["objects"][f"mail:{fname}"] = {
            "id": o["id"], "sha256": hashlib.sha256(blobs[fname]).hexdigest(),
            "w": next((f[3] for f in FIXTURES if f[0] == fname), None),
            "h": next((f[4] for f in FIXTURES if f[0] == fname), None)}
        return o["id"]

    m1 = [mail_upload("ryuji", n) for n in ("exif6.jpg", "land517.png", "report.pdf")]
    st, b = req("POST", "/api/v1/mail/send",
                {"to": ["alice@wittyloong.com"], "subject": "种子邮件：附件测试",
                 "body": "三个附件：EXIF 竖图、横幅 PNG、PDF。", "object_ids": m1}, admin_tok)
    must("mail ryuji->alice", st == 201, f"{st} {b[:200]}")
    time.sleep(0.4)
    # alice finds rfc_msgid then replies with tall.png
    st, b = req("GET", "/api/v1/mail/folders", None, toks["alice"])
    inbox = next(f["id"] for f in json.loads(b) if f["kind"] == "inbox")
    st, b = req("GET", f"/api/v1/mail/messages?folder={inbox}", None, toks["alice"])
    hit = next(m for m in json.loads(b) if "附件测试" in m["subject"])
    st, b = req("GET", f"/api/v1/mail/messages/{hit['id']}", None, toks["alice"])
    rfc = json.loads(b).get("rfc_msgid")
    t2 = mail_upload("alice", "tall.png")
    st, b = req("POST", "/api/v1/mail/send",
                {"to": ["admin@wittyloong.com"], "subject": "Re: 种子邮件：附件测试",
                 "body": "回一张长图。", "in_reply_to": rfc or "", "object_ids": [t2]},
                toks["alice"])
    must("mail alice->ryuji reply", st == 201, f"{st} {b[:200]}")
    st, b = req("POST", "/api/v1/mail/send",
                {"to": ["carol@wittyloong.com"], "subject": "纯文本邮件",
                 "body": "没有附件的测试邮件。"}, admin_tok)
    must("mail ryuji->carol", st == 201, f"{st} {b[:200]}")
    print("mail seeded")

    os.makedirs(OUT_DIR, exist_ok=True)
    manifest["note"] = ("sticker shelf is per-device localStorage 'wittychat.stickers'; "
                        "manifest.stickers is the ready-to-inject JSON")
    with open(MANIFEST, "w", encoding="utf-8") as f:
        json.dump(manifest, f, ensure_ascii=False, indent=2)
    print(f"manifest -> {MANIFEST}")


if __name__ == "__main__":
    main()
