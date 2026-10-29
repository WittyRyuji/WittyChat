from __future__ import annotations

from fastapi import APIRouter, HTTPException, Query, Request
from sqlalchemy import func, select
from sqlalchemy.exc import IntegrityError

from wittychat_server import chat, models
from wittychat_server.deps import DB, CurrentUser, commit
from wittychat_server.serializers import channel_out, channels_out
from wittychat_shared import protocol as p

router = APIRouter(prefix="/channels", tags=["channels"])


@router.get("/browse", response_model=list[p.BrowseChannel])
async def browse(db: DB, user: CurrentUser) -> list[p.BrowseChannel]:
    channels = list(
        await db.scalars(
            select(models.Channel)
            .where(models.Channel.type == p.ChannelType.PUBLIC, models.Channel.archived_at.is_(None))
            .order_by(func.lower(models.Channel.name))
        )
    )
    outs = await channels_out(db, channels)
    return [p.BrowseChannel(channel=c, member_count=len(c.member_ids), joined=user.id in c.member_ids) for c in outs]


@router.post("", response_model=p.Channel, status_code=201)
async def create(req: p.CreateChannelRequest, request: Request, db: DB, user: CurrentUser) -> p.Channel:
    channel = await chat.create_channel(db, user, req)
    out = await channel_out(db, channel)
    await commit(request, db)
    return out


@router.post("/dm", response_model=p.Channel)
async def open_dm(req: p.OpenDmRequest, request: Request, db: DB, user: CurrentUser) -> p.Channel:
    try:
        channel = await chat.open_dm(db, user, req.user_id)
        out = await channel_out(db, channel)
        await commit(request, db)
    except IntegrityError:
        # Lost a race creating the same DM; return the winner's channel.
        await db.rollback()
        channel = await chat.find_dm(db, user.id, req.user_id)
        assert channel is not None
        out = await channel_out(db, channel)
    return out


@router.get("/{channel_id}", response_model=p.Channel)
async def get(channel_id: int, db: DB, user: CurrentUser) -> p.Channel:
    channel = await chat.get_channel(db, channel_id)
    if not await chat.can_read(db, channel, user):
        raise HTTPException(404, "channel not found")
    return await channel_out(db, channel)


@router.patch("/{channel_id}", response_model=p.Channel)
async def update(channel_id: int, req: p.UpdateChannelRequest, request: Request, db: DB, user: CurrentUser) -> p.Channel:
    channel = await chat.get_channel(db, channel_id)
    await chat.update_channel(db, user, channel, req)
    out = await channel_out(db, channel)
    await commit(request, db)
    return out


@router.post("/{channel_id}/join", response_model=p.Channel)
async def join(channel_id: int, request: Request, db: DB, user: CurrentUser) -> p.Channel:
    channel = await chat.get_channel(db, channel_id)
    await chat.join_channel(db, user, channel)
    out = await channel_out(db, channel)
    await commit(request, db)
    return out


@router.post("/{channel_id}/leave", status_code=204)
async def leave(channel_id: int, request: Request, db: DB, user: CurrentUser) -> None:
    channel = await chat.get_channel(db, channel_id)
    await chat.leave_channel(db, user, channel)
    await commit(request, db)


@router.post("/{channel_id}/members", response_model=p.Channel)
async def add_members(channel_id: int, req: p.AddMembersRequest, request: Request, db: DB, user: CurrentUser) -> p.Channel:
    channel = await chat.get_channel(db, channel_id)
    await chat.add_members(db, user, channel, req.user_ids)
    out = await channel_out(db, channel)
    await commit(request, db)
    return out


@router.patch("/{channel_id}/membership", response_model=p.Membership)
async def update_membership(
    channel_id: int, req: p.UpdateMembershipRequest, request: Request, db: DB, user: CurrentUser
) -> p.Membership:
    channel = await chat.get_channel(db, channel_id)
    out = await chat.set_muted(db, user, channel, req.muted)
    await commit(request, db)
    return out


@router.post("/{channel_id}/read", response_model=p.Membership)
async def mark_read(channel_id: int, req: p.MarkReadRequest, request: Request, db: DB, user: CurrentUser) -> p.Membership:
    channel = await chat.get_channel(db, channel_id)
    out = await chat.mark_read(db, user, channel, req.seq)
    await commit(request, db)
    return out


@router.get("/{channel_id}/messages", response_model=p.HistoryResponse)
async def history(
    channel_id: int,
    db: DB,
    user: CurrentUser,
    before_seq: int | None = Query(None, ge=1),
    after_seq: int | None = Query(None, ge=0),
    limit: int = Query(50, ge=1, le=200),
) -> p.HistoryResponse:
    channel = await chat.get_channel(db, channel_id)
    return await chat.history(db, user, channel, before_seq=before_seq, after_seq=after_seq, limit=limit)
