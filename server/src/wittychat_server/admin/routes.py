"""Web admin console: server-rendered pages, plain HTML forms, no JavaScript build."""

from __future__ import annotations

import secrets
from pathlib import Path
from typing import Annotated
from urllib.parse import quote

from fastapi import APIRouter, Depends, Form, HTTPException, Request
from fastapi.responses import HTMLResponse, RedirectResponse
from fastapi.templating import Jinja2Templates
from sqlalchemy import func, select

from wittychat_server import chat, models
from wittychat_server.deps import DB, AppSettings, commit
from wittychat_server.events import emit
from wittychat_server.instance import get_instance_id
from wittychat_server.oidc import OidcError, pkce_pair
from wittychat_server.serializers import user_out
from wittychat_server.users import Profile, profile_from_claims, set_user_active, upsert_identity_user
from wittychat_shared import protocol as p

router = APIRouter(prefix="/admin", include_in_schema=False)
templates = Jinja2Templates(directory=Path(__file__).parent / "templates")


async def admin_user(request: Request, db: DB) -> models.User:
    uid = request.session.get("admin_uid")
    user = await db.get(models.User, uid) if uid else None
    if user is None or not user.is_active or not user.is_admin:
        request.session.pop("admin_uid", None)
        raise HTTPException(303, headers={"Location": "/admin/login"})
    return user


Admin = Annotated[models.User, Depends(admin_user)]


def _csrf_token(request: Request) -> str:
    if "csrf" not in request.session:
        request.session["csrf"] = secrets.token_urlsafe(24)
    return request.session["csrf"]


async def verify_csrf(request: Request, csrf: Annotated[str, Form()]) -> None:
    if not secrets.compare_digest(csrf, request.session.get("csrf", "")):
        raise HTTPException(400, "invalid CSRF token")


CSRF = Depends(verify_csrf)


def render(request: Request, name: str, status_code: int = 200, **context) -> HTMLResponse:
    return templates.TemplateResponse(
        request, name, {"csrf": _csrf_token(request), **context}, status_code=status_code
    )


def _back(path: str) -> RedirectResponse:
    return RedirectResponse(path, status_code=303)


# ------------------------------------------------------------------ auth


@router.get("/login")
async def login_page(request: Request, settings: AppSettings, error: str | None = None) -> HTMLResponse:
    return render(
        request, "login.html", oidc_enabled=settings.oidc_enabled, dev_login=settings.dev_login, error=error
    )


@router.get("/login/oidc")
async def login_oidc(request: Request, settings: AppSettings) -> RedirectResponse:
    oidc = request.app.state.oidc
    if oidc is None:
        raise HTTPException(404, "OIDC login is not configured")
    verifier, challenge = pkce_pair()
    state, nonce = secrets.token_urlsafe(24), secrets.token_urlsafe(24)
    request.session["oidc"] = {"state": state, "nonce": nonce, "verifier": verifier}
    url = await oidc.authorization_url(
        redirect_uri=f"{settings.public_url.rstrip('/')}/admin/auth/callback",
        state=state,
        nonce=nonce,
        code_challenge=challenge,
    )
    return RedirectResponse(url, status_code=303)


async def _finish_login(request: Request, db: DB, user: models.User) -> HTMLResponse | RedirectResponse:
    await commit(request, db)
    if not user.is_active or not user.is_admin:
        return render(request, "login.html", status_code=403, error="该账号没有管理员权限。")
    request.session["admin_uid"] = user.id
    return _back("/admin/")


@router.get("/auth/callback")
async def auth_callback(
    request: Request, db: DB, settings: AppSettings, code: str = "", state: str = "", error: str = ""
) -> HTMLResponse | RedirectResponse:
    oidc = request.app.state.oidc
    pending = request.session.pop("oidc", None)
    if oidc is None or error or not pending or not secrets.compare_digest(state, pending["state"]):
        return _back("/admin/login?error=" + quote("登录被取消" if error else "登录状态已失效，请重试"))
    try:
        claims = await oidc.login(
            code=code,
            code_verifier=pending["verifier"],
            redirect_uri=f"{settings.public_url.rstrip('/')}/admin/auth/callback",
            nonce=pending["nonce"],
        )
        oidc.check_allowed(claims)
    except OidcError as exc:
        return render(request, "login.html", status_code=401, error=f"登录失败：{exc}")
    user = await upsert_identity_user(
        db, settings, settings.oidc_provider_name, claims["sub"], profile_from_claims(claims), bootstrap_admin=True
    )
    return await _finish_login(request, db, user)


@router.post("/login/dev", dependencies=[CSRF])
async def login_dev(
    request: Request, db: DB, settings: AppSettings, username: Annotated[str, Form()]
) -> HTMLResponse | RedirectResponse:
    if not settings.dev_login:
        raise HTTPException(404, "not found")
    try:
        req = p.DevLoginRequest(username=username)
    except ValueError:
        return render(request, "login.html", status_code=400, error="用户名只能包含字母、数字和 _ . -")
    user = await upsert_identity_user(
        db, settings, "dev", req.username.lower(), Profile(username=req.username, display_name=req.username),
        bootstrap_admin=True,
    )
    return await _finish_login(request, db, user)


@router.post("/logout", dependencies=[CSRF])
async def logout(request: Request) -> RedirectResponse:
    request.session.clear()
    return _back("/admin/login")


# ------------------------------------------------------------------ pages


@router.get("/")
async def dashboard(request: Request, db: DB, admin: Admin) -> HTMLResponse:
    stats = {
        "users": await db.scalar(select(func.count()).select_from(models.User).where(models.User.is_active.is_(True))),
        "channels": await db.scalar(
            select(func.count()).select_from(models.Channel).where(models.Channel.type != p.ChannelType.DM)
        ),
        "dms": await db.scalar(
            select(func.count()).select_from(models.Channel).where(models.Channel.type == p.ChannelType.DM)
        ),
        "messages": await db.scalar(select(func.count()).select_from(models.Message)),
        "storage_bytes": await db.scalar(select(func.coalesce(func.sum(models.Blob.size), 0))),
        "online": len(request.app.state.hub.online_user_ids()),
    }
    return render(request, "dashboard.html", admin=admin, stats=stats, nav="dashboard")


@router.get("/users")
async def users_page(request: Request, db: DB, admin: Admin) -> HTMLResponse:
    users = list(await db.scalars(select(models.User).order_by(models.User.is_active.desc(), models.User.username)))
    providers: dict[int, list[str]] = {}
    for user_id, provider in await db.execute(select(models.UserIdentity.user_id, models.UserIdentity.provider)):
        providers.setdefault(user_id, []).append(provider)
    return render(request, "users.html", admin=admin, users=users, providers=providers, nav="users")


@router.post("/users/{user_id}/active", dependencies=[CSRF])
async def toggle_active(user_id: int, request: Request, db: DB, admin: Admin) -> RedirectResponse:
    user = await db.get(models.User, user_id)
    if user is None:
        raise HTTPException(404, "user not found")
    if user.id == admin.id:
        raise HTTPException(400, "cannot deactivate yourself")
    await set_user_active(db, user, not user.is_active)
    await commit(request, db)
    return _back("/admin/users")


@router.post("/users/{user_id}/admin", dependencies=[CSRF])
async def toggle_admin(user_id: int, request: Request, db: DB, admin: Admin) -> RedirectResponse:
    user = await db.get(models.User, user_id)
    if user is None:
        raise HTTPException(404, "user not found")
    if user.id == admin.id:
        raise HTTPException(400, "cannot change your own admin role")
    user.is_admin = not user.is_admin
    await db.flush()
    await emit(db, p.EventType.USER_UPSERTED, user_out(user))
    await commit(request, db)
    return _back("/admin/users")


@router.get("/channels")
async def channels_page(request: Request, db: DB, admin: Admin) -> HTMLResponse:
    member_counts = (
        select(models.ChannelMember.channel_id, func.count().label("members"))
        .group_by(models.ChannelMember.channel_id)
        .subquery()
    )
    rows = (
        await db.execute(
            select(models.Channel, func.coalesce(member_counts.c.members, 0))
            .outerjoin(member_counts, member_counts.c.channel_id == models.Channel.id)
            .where(models.Channel.type != p.ChannelType.DM)
            .order_by(models.Channel.archived_at.is_not(None), func.lower(models.Channel.name))
        )
    ).all()
    return render(request, "channels.html", admin=admin, rows=rows, nav="channels")


@router.post("/channels/{channel_id}/archive", dependencies=[CSRF])
async def toggle_archive(channel_id: int, request: Request, db: DB, admin: Admin) -> RedirectResponse:
    channel = await chat.get_channel(db, channel_id)
    if channel.type == p.ChannelType.DM:
        raise HTTPException(400, "direct messages cannot be archived")
    await chat.set_archived(db, channel, channel.archived_at is None)
    await commit(request, db)
    return _back("/admin/channels")


@router.get("/settings")
async def settings_page(request: Request, db: DB, admin: Admin, settings: AppSettings) -> HTMLResponse:
    state = request.app.state
    return render(
        request,
        "settings.html",
        admin=admin,
        nav="settings",
        settings=settings,
        storage=state.storage.describe(),
        instance_id=await get_instance_id(db),
        connections=state.hub.connection_count,
    )
