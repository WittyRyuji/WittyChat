from __future__ import annotations

from collections.abc import AsyncIterator
from typing import Annotated

from fastapi import Depends, HTTPException, Request
from fastapi.security import HTTPAuthorizationCredentials, HTTPBearer
from sqlalchemy.ext.asyncio import AsyncSession

from wittychat_server import models
from wittychat_server.config import Settings
from wittychat_server.security import resolve_token

_bearer = HTTPBearer(auto_error=False)


def get_settings(request: Request) -> Settings:
    return request.app.state.settings


async def get_db(request: Request) -> AsyncIterator[AsyncSession]:
    async with request.app.state.sessionmaker() as session:
        yield session


DB = Annotated[AsyncSession, Depends(get_db)]
AppSettings = Annotated[Settings, Depends(get_settings)]


async def current_session(
    db: DB,
    settings: AppSettings,
    creds: Annotated[HTTPAuthorizationCredentials | None, Depends(_bearer)],
) -> tuple[models.Session, models.User]:
    resolved = await resolve_token(db, creds.credentials, settings) if creds else None
    if resolved is None:
        raise HTTPException(401, "not authenticated", headers={"WWW-Authenticate": "Bearer"})
    return resolved


async def current_user(session: Annotated[tuple[models.Session, models.User], Depends(current_session)]) -> models.User:
    return session[1]


CurrentUser = Annotated[models.User, Depends(current_user)]


async def commit(request: Request, db: AsyncSession) -> None:
    """Commit and wake this worker's dispatcher (other workers are woken via NOTIFY)."""
    await db.commit()
    request.app.state.dispatcher.poke()
