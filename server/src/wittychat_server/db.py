from __future__ import annotations

from datetime import UTC, datetime

from sqlalchemy import BigInteger, DateTime, Integer, MetaData, event
from sqlalchemy.engine import Engine
from sqlalchemy.ext.asyncio import AsyncEngine, AsyncSession, async_sessionmaker, create_async_engine
from sqlalchemy.orm import DeclarativeBase
from sqlalchemy.types import TypeDecorator

# BIGINT on PostgreSQL; SQLite only auto-increments "INTEGER PRIMARY KEY".
BigIntPK = BigInteger().with_variant(Integer(), "sqlite")


def utcnow() -> datetime:
    return datetime.now(UTC)


class UTCDateTime(TypeDecorator[datetime]):
    """Timezone-aware UTC datetimes on every backend (SQLite drops tzinfo)."""

    impl = DateTime(timezone=True)
    cache_ok = True

    def process_bind_param(self, value: datetime | None, dialect) -> datetime | None:
        if value is not None and value.tzinfo is None:
            value = value.replace(tzinfo=UTC)
        return value

    def process_result_value(self, value: datetime | None, dialect) -> datetime | None:
        if value is not None and value.tzinfo is None:
            value = value.replace(tzinfo=UTC)
        return value


class Base(DeclarativeBase):
    metadata = MetaData(
        naming_convention={
            "ix": "ix_%(column_0_label)s",
            "uq": "uq_%(table_name)s_%(column_0_N_name)s",
            "ck": "ck_%(table_name)s_%(constraint_name)s",
            "fk": "fk_%(table_name)s_%(column_0_name)s_%(referred_table_name)s",
            "pk": "pk_%(table_name)s",
        }
    )


@event.listens_for(Engine, "connect")
def _sqlite_pragmas(dbapi_connection, connection_record) -> None:
    if "sqlite" in type(dbapi_connection).__module__:
        cursor = dbapi_connection.cursor()
        cursor.execute("PRAGMA foreign_keys=ON")
        cursor.execute("PRAGMA journal_mode=WAL")
        cursor.close()


def make_engine(url: str) -> AsyncEngine:
    kwargs: dict = {"pool_pre_ping": True}
    if url.startswith("postgresql"):
        kwargs.update(pool_size=10, max_overflow=10)
    return create_async_engine(url, **kwargs)


def make_sessionmaker(engine: AsyncEngine) -> async_sessionmaker[AsyncSession]:
    return async_sessionmaker(engine, expire_on_commit=False)
