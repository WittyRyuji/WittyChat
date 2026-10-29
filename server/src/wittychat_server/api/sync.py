from __future__ import annotations

from fastapi import APIRouter, Query
from sqlalchemy import select

from wittychat_server import models
from wittychat_server.deps import DB, CurrentUser
from wittychat_server.events import events_since, max_event_id
from wittychat_server.instance import get_instance_id
from wittychat_server.serializers import channels_out, memberships_out, user_out
from wittychat_shared import protocol as p

router = APIRouter(tags=["sync"])


@router.get("/sync/snapshot", response_model=p.Snapshot)
async def snapshot(db: DB, user: CurrentUser) -> p.Snapshot:
    # Read the cursor first: anything committed later is replayed on top (events are idempotent upserts).
    cursor = await max_event_id(db)
    members = list(await db.scalars(select(models.ChannelMember).where(models.ChannelMember.user_id == user.id)))
    channels = list(
        await db.scalars(select(models.Channel).where(models.Channel.id.in_([m.channel_id for m in members])))
    )
    users = list(await db.scalars(select(models.User).order_by(models.User.id)))
    return p.Snapshot(
        instance_id=await get_instance_id(db),
        cursor=cursor,
        me=user_out(user),
        users=[user_out(u) for u in users],
        channels=await channels_out(db, channels),
        memberships=await memberships_out(db, user.id, members),
    )


@router.get("/sync", response_model=p.SyncResponse)
async def sync(db: DB, user: CurrentUser, cursor: int = Query(ge=0), limit: int = Query(500, ge=1, le=1000)) -> p.SyncResponse:
    return await events_since(db, user.id, cursor, limit)
