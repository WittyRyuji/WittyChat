from __future__ import annotations

from pydantic import BaseModel
from sqlalchemy import and_, func, or_, select, text
from sqlalchemy.ext.asyncio import AsyncSession

from wittychat_server import models
from wittychat_server.serializers import event_out
from wittychat_shared import protocol as p

EVENT_LOCK_KEY = 0x57434854  # "WCHT"
NOTIFY_CHANNEL = "wittychat_events"
EPHEMERAL_CHANNEL = "wittychat_ephemeral"


async def emit(
    db: AsyncSession,
    type: p.EventType,
    payload: BaseModel | dict,
    *,
    channel_id: int | None = None,
    user_id: int | None = None,
) -> models.Event:
    """Append an event inside the caller's transaction.

    On PostgreSQL a transaction-scoped advisory lock serializes "allocate id -> commit",
    so event ids become visible strictly in id order and a reader's cursor never skips
    an event that commits late. SQLite already serializes writers.
    """
    if db.get_bind().dialect.name == "postgresql":
        await db.execute(text("SELECT pg_advisory_xact_lock(:key)"), {"key": EVENT_LOCK_KEY})
        await db.execute(text("SELECT pg_notify(:channel, '')"), {"channel": NOTIFY_CHANNEL})
    data = payload.model_dump(mode="json") if isinstance(payload, BaseModel) else payload
    event = models.Event(type=type.value, channel_id=channel_id, user_id=user_id, payload=data)
    db.add(event)
    await db.flush()
    return event


def visible_to(user_id: int):
    member_channels = select(models.ChannelMember.channel_id).where(models.ChannelMember.user_id == user_id)
    ev = models.Event
    return or_(
        ev.user_id == user_id,
        and_(ev.user_id.is_(None), or_(ev.channel_id.is_(None), ev.channel_id.in_(member_channels))),
    )


async def max_event_id(db: AsyncSession) -> int:
    return (await db.scalar(select(func.max(models.Event.id)))) or 0


async def events_since(
    db: AsyncSession, user_id: int, cursor: int, limit: int = 500
) -> p.SyncResponse:
    head = await max_event_id(db)
    if cursor > head:
        return p.SyncResponse(events=[], cursor=head, reset=True)
    rows = (
        await db.scalars(
            select(models.Event)
            .where(models.Event.id > cursor, visible_to(user_id))
            .order_by(models.Event.id)
            .limit(limit + 1)
        )
    ).all()
    has_more = len(rows) > limit
    rows = rows[:limit]
    # Without more visible rows the cursor may jump to head, skipping invisible events.
    new_cursor = rows[-1].id if has_more else max(head, rows[-1].id if rows else cursor)
    return p.SyncResponse(events=[event_out(r) for r in rows], cursor=new_cursor, has_more=has_more)
