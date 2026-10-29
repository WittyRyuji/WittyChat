from __future__ import annotations

import hashlib
import secrets
from datetime import timedelta

from sqlalchemy import select, update
from sqlalchemy.ext.asyncio import AsyncSession

from wittychat_server import models
from wittychat_server.config import Settings
from wittychat_server.db import utcnow

SLIDING_REFRESH = timedelta(minutes=5)


def new_token() -> str:
    return secrets.token_urlsafe(32)


def hash_token(token: str) -> str:
    return hashlib.sha256(token.encode()).hexdigest()


async def create_session(
    db: AsyncSession,
    user: models.User,
    settings: Settings,
    *,
    user_agent: str | None = None,
    ip: str | None = None,
) -> str:
    token = new_token()
    now = utcnow()
    db.add(
        models.Session(
            user_id=user.id,
            token_hash=hash_token(token),
            created_at=now,
            last_seen_at=now,
            expires_at=now + timedelta(days=settings.session_ttl_days),
            user_agent=(user_agent or "")[:255] or None,
            ip=ip,
        )
    )
    user.last_login_at = now
    return token


async def resolve_token(
    db: AsyncSession, token: str, settings: Settings
) -> tuple[models.Session, models.User] | None:
    row = (
        await db.execute(
            select(models.Session, models.User)
            .join(models.User, models.User.id == models.Session.user_id)
            .where(models.Session.token_hash == hash_token(token))
        )
    ).first()
    if row is None:
        return None
    session, user = row
    now = utcnow()
    if session.revoked_at is not None or session.expires_at <= now or not user.is_active:
        return None
    if now - session.last_seen_at > SLIDING_REFRESH:
        session.last_seen_at = now
        session.expires_at = now + timedelta(days=settings.session_ttl_days)
        await db.commit()
    return session, user


async def revoke_user_sessions(db: AsyncSession, user_id: int) -> None:
    await db.execute(
        update(models.Session)
        .where(models.Session.user_id == user_id, models.Session.revoked_at.is_(None))
        .values(revoked_at=utcnow())
    )
