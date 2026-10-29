"""Mirror the Pocket ID user directory: colleagues appear before their first login,
and accounts disabled or deleted in Pocket ID are deactivated here (sessions revoked)."""

from __future__ import annotations

import asyncio
import logging

import httpx
from sqlalchemy import select, text
from sqlalchemy.ext.asyncio import AsyncSession, async_sessionmaker

from wittychat_server import models
from wittychat_server.broadcast import Dispatcher
from wittychat_server.config import Settings
from wittychat_server.users import Profile, set_user_active, upsert_identity_user

log = logging.getLogger(__name__)

DIRECTORY_LOCK_KEY = 0x57434844  # "WCHD"


async def fetch_pocketid_users(settings: Settings, http: httpx.AsyncClient) -> list[dict]:
    assert settings.oidc_issuer and settings.pocketid_api_key
    base = settings.oidc_issuer.rstrip("/")
    users: list[dict] = []
    page = 1
    while True:
        resp = await http.get(
            f"{base}/api/users",
            params={"pagination[page]": page, "pagination[limit]": 100},
            headers={"X-API-KEY": settings.pocketid_api_key},
        )
        resp.raise_for_status()
        data = resp.json()
        users.extend(data["data"])
        if page >= data["pagination"]["totalPages"]:
            return users
        page += 1


def _profile(entry: dict) -> Profile:
    username = entry.get("username") or (entry.get("email") or "").split("@")[0] or entry["id"]
    full_name = " ".join(filter(None, [entry.get("firstName"), entry.get("lastName")]))
    return Profile(
        username=username,
        display_name=entry.get("displayName") or full_name or username,
        email=entry.get("email") or None,
        groups=[g["name"] for g in entry.get("userGroups") or [] if g.get("name")],
        is_active=not entry.get("disabled", False),
    )


async def apply_directory(db: AsyncSession, settings: Settings, entries: list[dict]) -> None:
    provider = settings.oidc_provider_name
    seen: set[str] = set()
    for entry in entries:
        seen.add(entry["id"])
        await upsert_identity_user(db, settings, provider, entry["id"], _profile(entry))
    if not seen:
        return  # an empty directory is far more likely a misconfiguration than reality
    # Only reached after a complete fetch, so missing ids really were deleted upstream.
    stale = await db.scalars(
        select(models.User)
        .join(models.UserIdentity, models.UserIdentity.user_id == models.User.id)
        .where(
            models.UserIdentity.provider == provider,
            models.UserIdentity.subject.not_in(seen),
            models.User.is_active.is_(True),
        )
    )
    for user in list(stale):
        await set_user_active(db, user, False)


async def sync_directory(
    settings: Settings, sessionmaker: async_sessionmaker[AsyncSession], http: httpx.AsyncClient, dispatcher: Dispatcher
) -> None:
    entries = await fetch_pocketid_users(settings, http)
    async with sessionmaker() as db:
        if db.get_bind().dialect.name == "postgresql":
            # One worker per round is enough.
            if not await db.scalar(text("SELECT pg_try_advisory_xact_lock(:k)"), {"k": DIRECTORY_LOCK_KEY}):
                return
        await apply_directory(db, settings, entries)
        await db.commit()
    dispatcher.poke()


async def directory_loop(
    settings: Settings, sessionmaker: async_sessionmaker[AsyncSession], http: httpx.AsyncClient, dispatcher: Dispatcher
) -> None:
    while True:
        try:
            await sync_directory(settings, sessionmaker, http, dispatcher)
        except asyncio.CancelledError:
            raise
        except Exception:
            log.exception("Pocket ID directory sync failed")
        await asyncio.sleep(settings.directory_sync_interval)
