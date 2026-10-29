from __future__ import annotations

import asyncio

from alembic import context
from sqlalchemy.ext.asyncio import create_async_engine

from wittychat_server import models  # noqa: F401  (registers tables)
from wittychat_server.db import Base

config = context.config
target_metadata = Base.metadata


def _url() -> str:
    url = config.get_main_option("sqlalchemy.url")
    if not url:
        from wittychat_server.config import get_settings

        url = get_settings().database_url
    return url


def run_migrations_offline() -> None:
    context.configure(url=_url(), target_metadata=target_metadata, literal_binds=True, render_as_batch=True)
    with context.begin_transaction():
        context.run_migrations()


def _run(connection) -> None:
    context.configure(connection=connection, target_metadata=target_metadata, render_as_batch=True)
    with context.begin_transaction():
        context.run_migrations()


async def run_migrations_online() -> None:
    engine = create_async_engine(_url())
    async with engine.connect() as connection:
        await connection.run_sync(_run)
    await engine.dispose()


if context.is_offline_mode():
    run_migrations_offline()
else:
    asyncio.run(run_migrations_online())
