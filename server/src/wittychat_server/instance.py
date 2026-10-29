from __future__ import annotations

import uuid

from sqlalchemy.exc import IntegrityError
from sqlalchemy.ext.asyncio import AsyncSession, async_sessionmaker

from wittychat_server import models

INSTANCE_KEY = "instance_id"


async def ensure_instance_id(sessionmaker: async_sessionmaker[AsyncSession]) -> str:
    """A stable id per database; clients drop their cache when it changes."""
    async with sessionmaker() as db:
        if (row := await db.get(models.ServerMeta, INSTANCE_KEY)) is not None:
            return row.value
        db.add(models.ServerMeta(key=INSTANCE_KEY, value=uuid.uuid4().hex))
        try:
            await db.commit()
        except IntegrityError:
            await db.rollback()
        return (await db.get_one(models.ServerMeta, INSTANCE_KEY)).value


async def get_instance_id(db: AsyncSession) -> str:
    return (await db.get_one(models.ServerMeta, INSTANCE_KEY)).value
