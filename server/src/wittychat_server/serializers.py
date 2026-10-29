from __future__ import annotations

from collections import defaultdict
from collections.abc import Sequence

from sqlalchemy import func, select
from sqlalchemy.ext.asyncio import AsyncSession

from wittychat_server import models
from wittychat_shared import protocol as p


def user_out(user: models.User) -> p.User:
    return p.User.model_validate(user)


def attachment_out(att: models.Attachment) -> p.Attachment:
    return p.Attachment(
        id=att.id, filename=att.filename, content_type=att.content_type, size=att.size, sha256=att.blob_sha256
    )


async def channels_out(db: AsyncSession, channels: Sequence[models.Channel]) -> list[p.Channel]:
    members: dict[int, list[int]] = defaultdict(list)
    if channels:
        rows = await db.execute(
            select(models.ChannelMember.channel_id, models.ChannelMember.user_id).where(
                models.ChannelMember.channel_id.in_([c.id for c in channels])
            )
        )
        for channel_id, user_id in rows:
            members[channel_id].append(user_id)
    return [
        p.Channel(
            id=c.id,
            type=p.ChannelType(c.type),
            name=c.name,
            topic=c.topic,
            last_seq=c.last_seq,
            last_message_at=c.last_message_at,
            archived=c.archived_at is not None,
            member_ids=sorted(members[c.id]),
            created_at=c.created_at,
        )
        for c in channels
    ]


async def channel_out(db: AsyncSession, channel: models.Channel) -> p.Channel:
    return (await channels_out(db, [channel]))[0]


async def messages_out(db: AsyncSession, messages: Sequence[models.Message]) -> list[p.Message]:
    attachments: dict[int, list[p.Attachment]] = defaultdict(list)
    mentions: dict[int, list[int]] = defaultdict(list)
    ids = [m.id for m in messages]
    if ids:
        for att in await db.scalars(
            select(models.Attachment).where(models.Attachment.message_id.in_(ids)).order_by(models.Attachment.id)
        ):
            attachments[att.message_id].append(attachment_out(att))
        for message_id, user_id in await db.execute(
            select(models.MessageMention.message_id, models.MessageMention.user_id).where(
                models.MessageMention.message_id.in_(ids)
            )
        ):
            mentions[message_id].append(user_id)
    return [
        p.Message(
            id=m.id,
            channel_id=m.channel_id,
            seq=m.seq,
            sender_id=m.sender_id,
            content="" if m.deleted_at else m.content,
            client_msg_id=m.client_msg_id,
            mentions=[] if m.deleted_at else sorted(mentions[m.id]),
            attachments=[] if m.deleted_at else attachments[m.id],
            created_at=m.created_at,
            edited_at=m.edited_at,
            deleted=m.deleted_at is not None,
        )
        for m in messages
    ]


async def message_out(db: AsyncSession, message: models.Message) -> p.Message:
    return (await messages_out(db, [message]))[0]


async def mention_counts(db: AsyncSession, user_id: int, channel_ids: Sequence[int] | None = None) -> dict[int, int]:
    mm, cm, msg = models.MessageMention, models.ChannelMember, models.Message
    query = (
        select(mm.channel_id, func.count())
        .join(cm, (cm.channel_id == mm.channel_id) & (cm.user_id == mm.user_id))
        .join(msg, msg.id == mm.message_id)
        .where(mm.user_id == user_id, mm.seq > cm.last_read_seq, msg.deleted_at.is_(None))
        .group_by(mm.channel_id)
    )
    if channel_ids is not None:
        query = query.where(mm.channel_id.in_(channel_ids))
    return dict((await db.execute(query)).all())


async def memberships_out(db: AsyncSession, user_id: int, members: Sequence[models.ChannelMember]) -> list[p.Membership]:
    counts = await mention_counts(db, user_id, [m.channel_id for m in members])
    return [
        p.Membership(
            channel_id=m.channel_id,
            role=p.MemberRole(m.role),
            last_read_seq=m.last_read_seq,
            muted=m.muted,
            mention_count=counts.get(m.channel_id, 0),
        )
        for m in members
    ]


def event_out(ev: models.Event) -> p.Event:
    return p.Event(id=ev.id, type=p.EventType(ev.type), channel_id=ev.channel_id, payload=ev.payload, created_at=ev.created_at)
