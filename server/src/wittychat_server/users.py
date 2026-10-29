from __future__ import annotations

import re
from dataclasses import dataclass

from sqlalchemy import exists, func, select
from sqlalchemy.ext.asyncio import AsyncSession

from wittychat_server import models
from wittychat_server.config import Settings
from wittychat_server.events import emit
from wittychat_server.security import revoke_user_sessions
from wittychat_server.serializers import user_out
from wittychat_shared import protocol as p

_INVALID_USERNAME_CHARS = re.compile(r"[^A-Za-z0-9_.\-]")


@dataclass
class Profile:
    username: str
    display_name: str
    email: str | None = None
    avatar_url: str | None = None
    groups: list[str] | None = None
    is_active: bool | None = None


def profile_from_claims(claims: dict) -> Profile:
    email = claims.get("email") or None
    username = claims.get("preferred_username") or (email.split("@")[0] if email else "") or claims["sub"]
    groups = claims.get("groups")
    return Profile(
        username=username,
        display_name=claims.get("display_name") or claims.get("name") or username,
        email=email,
        avatar_url=claims.get("picture") or None,
        groups=[str(g) for g in groups] if isinstance(groups, list) else None,
    )


def sanitize_username(raw: str) -> str:
    return _INVALID_USERNAME_CHARS.sub("_", raw.strip())[:64] or "user"


async def _unique_username(db: AsyncSession, base: str, exclude_user_id: int | None = None) -> str:
    candidate, n = base, 1
    while True:
        query = select(models.User.id).where(func.lower(models.User.username) == candidate.lower())
        if exclude_user_id is not None:
            query = query.where(models.User.id != exclude_user_id)
        if await db.scalar(query) is None:
            return candidate
        n += 1
        candidate = f"{base[:60]}{n}"


async def upsert_identity_user(
    db: AsyncSession,
    settings: Settings,
    provider: str,
    subject: str,
    profile: Profile,
    *,
    bootstrap_admin: bool = False,
) -> models.User:
    """Create or refresh the local user behind an external identity; emits user.upserted on change."""
    identity = await db.scalar(
        select(models.UserIdentity).where(
            models.UserIdentity.provider == provider, models.UserIdentity.subject == subject
        )
    )
    username = sanitize_username(profile.username)
    if identity is None:
        user = models.User(
            username=await _unique_username(db, username),
            display_name=profile.display_name[:128],
            is_active=True,
            is_admin=False,
        )
        db.add(user)
        await db.flush()
        db.add(models.UserIdentity(user_id=user.id, provider=provider, subject=subject))
        changed = True
    else:
        user = await db.get_one(models.User, identity.user_id)
        changed = False
        if user.username.lower() != username.lower():
            user.username = await _unique_username(db, username, exclude_user_id=user.id)
            changed = True

    for field, value in (
        ("display_name", profile.display_name[:128]),
        ("email", profile.email),
        ("avatar_url", profile.avatar_url),
    ):
        if value is not None and getattr(user, field) != value:
            setattr(user, field, value)
            changed = True

    if profile.is_active is not None and user.is_active != profile.is_active:
        user.is_active = profile.is_active
        changed = True
        if not profile.is_active:
            await revoke_user_sessions(db, user.id)

    if settings.admin_groups and profile.groups is not None:
        is_admin = bool(set(profile.groups) & set(settings.admin_groups))
        if user.is_admin != is_admin:
            user.is_admin = is_admin
            changed = True
    elif bootstrap_admin and not user.is_admin:
        # First interactive login becomes admin while no admin exists and no admin group is configured.
        if not await db.scalar(select(exists().where(models.User.is_admin.is_(True)))):
            user.is_admin = True
            changed = True

    await db.flush()
    if changed:
        await emit(db, p.EventType.USER_UPSERTED, user_out(user))
    return user


async def set_user_active(db: AsyncSession, user: models.User, active: bool) -> None:
    if user.is_active == active:
        return
    user.is_active = active
    if not active:
        await revoke_user_sessions(db, user.id)
    await db.flush()
    await emit(db, p.EventType.USER_UPSERTED, user_out(user))
