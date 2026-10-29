from __future__ import annotations

import re

from fastapi import HTTPException
from sqlalchemy import delete, func, select, update
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy.orm.attributes import set_committed_value

from wittychat_server import models
from wittychat_server.db import utcnow
from wittychat_server.events import emit
from wittychat_server.serializers import channel_out, memberships_out, message_out, messages_out
from wittychat_shared import protocol as p

MENTION_RE = re.compile(r"(?<![\w@])@([A-Za-z0-9_.\-]+)")


def _forbidden(detail: str) -> HTTPException:
    return HTTPException(403, detail)


async def get_channel(db: AsyncSession, channel_id: int) -> models.Channel:
    channel = await db.get(models.Channel, channel_id)
    if channel is None:
        raise HTTPException(404, "channel not found")
    return channel


async def get_member(db: AsyncSession, channel_id: int, user_id: int) -> models.ChannelMember | None:
    return await db.get(models.ChannelMember, (channel_id, user_id))


async def require_member(db: AsyncSession, channel: models.Channel, user: models.User) -> models.ChannelMember:
    member = await get_member(db, channel.id, user.id)
    if member is None:
        # Do not reveal private channels to outsiders.
        raise HTTPException(404, "channel not found") if channel.type != p.ChannelType.PUBLIC else _forbidden(
            "join the channel first"
        )
    return member


async def can_read(db: AsyncSession, channel: models.Channel, user: models.User) -> bool:
    return channel.type == p.ChannelType.PUBLIC or await get_member(db, channel.id, user.id) is not None


def _require_writable(channel: models.Channel) -> None:
    if channel.archived_at is not None:
        raise _forbidden("channel is archived")


async def _emit_channel(db: AsyncSession, channel: models.Channel) -> None:
    await db.flush()
    await emit(db, p.EventType.CHANNEL_UPSERTED, await channel_out(db, channel), channel_id=channel.id)


async def emit_membership(db: AsyncSession, member: models.ChannelMember) -> p.Membership:
    await db.flush()
    out = (await memberships_out(db, member.user_id, [member]))[0]
    await emit(db, p.EventType.MEMBERSHIP_UPDATED, out, channel_id=member.channel_id, user_id=member.user_id)
    return out


async def _active_user_ids(db: AsyncSession, user_ids: set[int]) -> set[int]:
    if not user_ids:
        return set()
    rows = await db.scalars(
        select(models.User.id).where(models.User.id.in_(user_ids), models.User.is_active.is_(True))
    )
    return set(rows)


# ------------------------------------------------------------------ channels


async def create_channel(db: AsyncSession, user: models.User, req: p.CreateChannelRequest) -> models.Channel:
    name = req.name.strip()
    if not name:
        raise HTTPException(422, "name must not be blank")
    duplicate = await db.scalar(
        select(models.Channel.id).where(
            models.Channel.type != p.ChannelType.DM, func.lower(models.Channel.name) == name.lower()
        )
    )
    if duplicate is not None:
        raise HTTPException(409, "a channel with this name already exists")
    channel = models.Channel(type=req.type.value, name=name, topic=req.topic.strip(), created_by=user.id, last_seq=0)
    db.add(channel)
    await db.flush()
    db.add(models.ChannelMember(channel_id=channel.id, user_id=user.id, role=p.MemberRole.OWNER.value))
    for uid in await _active_user_ids(db, set(req.member_ids) - {user.id}):
        db.add(models.ChannelMember(channel_id=channel.id, user_id=uid))
    await _emit_channel(db, channel)
    return channel


async def find_dm(db: AsyncSession, a: int, b: int) -> models.Channel | None:
    low, high = sorted((a, b))
    return await db.scalar(select(models.Channel).where(models.Channel.dm_key == f"{low}:{high}"))


async def open_dm(db: AsyncSession, user: models.User, other_id: int) -> models.Channel:
    if other_id != user.id and not await _active_user_ids(db, {other_id}):
        raise HTTPException(404, "user not found")
    if (existing := await find_dm(db, user.id, other_id)) is not None:
        return existing
    low, high = sorted((user.id, other_id))
    channel = models.Channel(type=p.ChannelType.DM.value, dm_key=f"{low}:{high}", created_by=user.id, last_seq=0)
    db.add(channel)
    await db.flush()
    for uid in {user.id, other_id}:
        db.add(models.ChannelMember(channel_id=channel.id, user_id=uid))
    await _emit_channel(db, channel)
    return channel


async def join_channel(db: AsyncSession, user: models.User, channel: models.Channel) -> None:
    if channel.type != p.ChannelType.PUBLIC:
        raise HTTPException(404, "channel not found")
    _require_writable(channel)
    if await get_member(db, channel.id, user.id) is None:
        db.add(models.ChannelMember(channel_id=channel.id, user_id=user.id, last_read_seq=channel.last_seq))
        await _emit_channel(db, channel)


async def leave_channel(db: AsyncSession, user: models.User, channel: models.Channel) -> None:
    if channel.type == p.ChannelType.DM:
        raise HTTPException(400, "cannot leave a direct message")
    await require_member(db, channel, user)
    await db.execute(
        delete(models.ChannelMember).where(
            models.ChannelMember.channel_id == channel.id, models.ChannelMember.user_id == user.id
        )
    )
    await _emit_channel(db, channel)
    await emit(db, p.EventType.CHANNEL_REMOVED, {"channel_id": channel.id}, channel_id=channel.id, user_id=user.id)


async def add_members(db: AsyncSession, user: models.User, channel: models.Channel, user_ids: list[int]) -> None:
    if channel.type == p.ChannelType.DM:
        raise HTTPException(400, "cannot add members to a direct message")
    _require_writable(channel)
    await require_member(db, channel, user)
    existing = set(
        await db.scalars(select(models.ChannelMember.user_id).where(models.ChannelMember.channel_id == channel.id))
    )
    added = await _active_user_ids(db, set(user_ids) - existing)
    for uid in added:
        db.add(models.ChannelMember(channel_id=channel.id, user_id=uid))
    if added:
        await _emit_channel(db, channel)


async def update_channel(
    db: AsyncSession, user: models.User, channel: models.Channel, req: p.UpdateChannelRequest
) -> None:
    if channel.type == p.ChannelType.DM:
        raise HTTPException(400, "direct messages have no settings")
    _require_writable(channel)
    member = await require_member(db, channel, user)
    if req.name is not None and req.name.strip() != channel.name:
        if member.role != p.MemberRole.OWNER and not user.is_admin:
            raise _forbidden("only the owner can rename the channel")
        name = req.name.strip()
        duplicate = await db.scalar(
            select(models.Channel.id).where(
                models.Channel.id != channel.id,
                models.Channel.type != p.ChannelType.DM,
                func.lower(models.Channel.name) == name.lower(),
            )
        )
        if duplicate is not None:
            raise HTTPException(409, "a channel with this name already exists")
        channel.name = name
    if req.topic is not None:
        channel.topic = req.topic.strip()
    await _emit_channel(db, channel)


async def set_archived(db: AsyncSession, channel: models.Channel, archived: bool) -> None:
    if (channel.archived_at is not None) == archived:
        return
    channel.archived_at = utcnow() if archived else None
    await _emit_channel(db, channel)


async def set_muted(db: AsyncSession, user: models.User, channel: models.Channel, muted: bool) -> p.Membership:
    member = await require_member(db, channel, user)
    member.muted = muted
    return await emit_membership(db, member)


async def mark_read(db: AsyncSession, user: models.User, channel: models.Channel, seq: int) -> p.Membership:
    member = await require_member(db, channel, user)
    seq = min(seq, channel.last_seq)
    if seq > member.last_read_seq:
        member.last_read_seq = seq
        return await emit_membership(db, member)
    return (await memberships_out(db, user.id, [member]))[0]


# ------------------------------------------------------------------ messages


async def _resolve_mentions(db: AsyncSession, channel_id: int, sender_id: int, content: str) -> list[int]:
    names = {n.lower() for n in MENTION_RE.findall(content)}
    if not names:
        return []
    rows = await db.scalars(
        select(models.User.id)
        .join(models.ChannelMember, models.ChannelMember.user_id == models.User.id)
        .where(
            models.ChannelMember.channel_id == channel_id,
            func.lower(models.User.username).in_(names),
            models.User.id != sender_id,
        )
    )
    return sorted(set(rows))


async def _set_mentions(db: AsyncSession, message: models.Message, user_ids: list[int]) -> None:
    await db.execute(delete(models.MessageMention).where(models.MessageMention.message_id == message.id))
    for uid in user_ids:
        db.add(models.MessageMention(message_id=message.id, user_id=uid, channel_id=message.channel_id, seq=message.seq))


async def send_message(
    db: AsyncSession, user: models.User, channel: models.Channel, req: p.SendMessageRequest
) -> tuple[models.Message, bool]:
    """Returns (message, created). Re-sending the same client_msg_id is idempotent."""
    member = await require_member(db, channel, user)
    existing = await db.scalar(
        select(models.Message).where(
            models.Message.sender_id == user.id, models.Message.client_msg_id == req.client_msg_id
        )
    )
    if existing is not None:
        if existing.channel_id != channel.id:
            raise HTTPException(409, "client_msg_id already used in another channel")
        return existing, False
    _require_writable(channel)

    attachment_ids = set(req.attachment_ids)
    attachments = list(
        await db.scalars(select(models.Attachment).where(models.Attachment.id.in_(attachment_ids)))
    ) if attachment_ids else []
    if len(attachments) != len(attachment_ids) or any(
        a.uploader_id != user.id or a.message_id is not None for a in attachments
    ):
        raise HTTPException(400, "invalid attachment")
    if not req.content.strip() and not attachments:
        raise HTTPException(422, "message is empty")

    now = utcnow()
    seq = (
        await db.execute(
            update(models.Channel)
            .where(models.Channel.id == channel.id)
            .values(last_seq=models.Channel.last_seq + 1, last_message_at=now)
            .returning(models.Channel.last_seq)
        )
    ).scalar_one()
    set_committed_value(channel, "last_seq", seq)
    set_committed_value(channel, "last_message_at", now)

    message = models.Message(
        channel_id=channel.id,
        seq=seq,
        sender_id=user.id,
        content=req.content,
        client_msg_id=req.client_msg_id,
        created_at=now,
    )
    db.add(message)
    await db.flush()
    for att in attachments:
        att.message_id = message.id
    await _set_mentions(db, message, await _resolve_mentions(db, channel.id, user.id, req.content))
    member.last_read_seq = seq
    await db.flush()
    await emit(db, p.EventType.MESSAGE_CREATED, await message_out(db, message), channel_id=channel.id)
    await emit_membership(db, member)
    return message, True


async def _own_message(db: AsyncSession, user: models.User, message_id: int, *, allow_admin: bool) -> models.Message:
    message = await db.get(models.Message, message_id)
    if message is None or message.deleted_at is not None:
        raise HTTPException(404, "message not found")
    channel = await get_channel(db, message.channel_id)
    if not await can_read(db, channel, user):
        raise HTTPException(404, "message not found")
    if message.sender_id != user.id and not (allow_admin and user.is_admin):
        raise _forbidden("not your message")
    _require_writable(channel)
    return message


async def edit_message(db: AsyncSession, user: models.User, message_id: int, content: str) -> models.Message:
    message = await _own_message(db, user, message_id, allow_admin=False)
    if not content.strip():
        raise HTTPException(422, "message is empty")
    message.content = content
    message.edited_at = utcnow()
    await _set_mentions(db, message, await _resolve_mentions(db, message.channel_id, user.id, content))
    await db.flush()
    await emit(db, p.EventType.MESSAGE_UPDATED, await message_out(db, message), channel_id=message.channel_id)
    return message


async def delete_message(db: AsyncSession, user: models.User, message_id: int) -> models.Message:
    message = await _own_message(db, user, message_id, allow_admin=True)
    message.deleted_at = utcnow()
    await db.flush()
    await emit(db, p.EventType.MESSAGE_DELETED, await message_out(db, message), channel_id=message.channel_id)
    return message


async def history(
    db: AsyncSession,
    user: models.User,
    channel: models.Channel,
    *,
    before_seq: int | None,
    after_seq: int | None,
    limit: int,
) -> p.HistoryResponse:
    if not await can_read(db, channel, user):
        raise HTTPException(404, "channel not found")
    query = select(models.Message).where(models.Message.channel_id == channel.id)
    if after_seq is not None:
        query = query.where(models.Message.seq > after_seq).order_by(models.Message.seq)
    else:
        if before_seq is not None:
            query = query.where(models.Message.seq < before_seq)
        query = query.order_by(models.Message.seq.desc())
    rows = list(await db.scalars(query.limit(limit + 1)))
    has_more = len(rows) > limit
    rows = rows[:limit]
    if after_seq is None:
        rows.reverse()
    return p.HistoryResponse(messages=await messages_out(db, rows), has_more=has_more)
