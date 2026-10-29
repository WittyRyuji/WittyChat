from __future__ import annotations

from datetime import datetime
from typing import Any

from sqlalchemy import (
    JSON,
    BigInteger,
    Boolean,
    ForeignKey,
    Index,
    String,
    Text,
    UniqueConstraint,
)
from sqlalchemy.dialects.postgresql import JSONB
from sqlalchemy.orm import Mapped, mapped_column

from wittychat_server.db import Base, BigIntPK, UTCDateTime, utcnow

JSONType = JSON().with_variant(JSONB(), "postgresql")


class ServerMeta(Base):
    __tablename__ = "server_meta"

    key: Mapped[str] = mapped_column(String(64), primary_key=True)
    value: Mapped[str] = mapped_column(Text)


class User(Base):
    __tablename__ = "users"

    id: Mapped[int] = mapped_column(BigIntPK, primary_key=True)
    username: Mapped[str] = mapped_column(String(64), unique=True)
    display_name: Mapped[str] = mapped_column(String(128))
    email: Mapped[str | None] = mapped_column(String(255))
    avatar_url: Mapped[str | None] = mapped_column(String(1024))
    is_active: Mapped[bool] = mapped_column(Boolean, default=True)
    is_admin: Mapped[bool] = mapped_column(Boolean, default=False)
    created_at: Mapped[datetime] = mapped_column(UTCDateTime, default=utcnow)
    updated_at: Mapped[datetime] = mapped_column(UTCDateTime, default=utcnow, onupdate=utcnow)
    last_login_at: Mapped[datetime | None] = mapped_column(UTCDateTime)


class UserIdentity(Base):
    """Link to an external identity (Pocket ID today, WittyChat's own IdP later)."""

    __tablename__ = "user_identities"
    __table_args__ = (UniqueConstraint("provider", "subject"),)

    id: Mapped[int] = mapped_column(BigIntPK, primary_key=True)
    user_id: Mapped[int] = mapped_column(ForeignKey("users.id", ondelete="CASCADE"), index=True)
    provider: Mapped[str] = mapped_column(String(64))
    subject: Mapped[str] = mapped_column(String(255))
    created_at: Mapped[datetime] = mapped_column(UTCDateTime, default=utcnow)


class Session(Base):
    __tablename__ = "sessions"

    id: Mapped[int] = mapped_column(BigIntPK, primary_key=True)
    user_id: Mapped[int] = mapped_column(ForeignKey("users.id", ondelete="CASCADE"), index=True)
    token_hash: Mapped[str] = mapped_column(String(64), unique=True)
    created_at: Mapped[datetime] = mapped_column(UTCDateTime, default=utcnow)
    last_seen_at: Mapped[datetime] = mapped_column(UTCDateTime, default=utcnow)
    expires_at: Mapped[datetime] = mapped_column(UTCDateTime)
    revoked_at: Mapped[datetime | None] = mapped_column(UTCDateTime)
    user_agent: Mapped[str | None] = mapped_column(String(255))
    ip: Mapped[str | None] = mapped_column(String(64))


class Channel(Base):
    __tablename__ = "channels"

    id: Mapped[int] = mapped_column(BigIntPK, primary_key=True)
    type: Mapped[str] = mapped_column(String(16))
    name: Mapped[str | None] = mapped_column(String(80))
    topic: Mapped[str] = mapped_column(String(250), default="")
    created_by: Mapped[int | None] = mapped_column(ForeignKey("users.id", ondelete="SET NULL"))
    created_at: Mapped[datetime] = mapped_column(UTCDateTime, default=utcnow)
    last_seq: Mapped[int] = mapped_column(BigInteger, default=0)
    last_message_at: Mapped[datetime | None] = mapped_column(UTCDateTime)
    archived_at: Mapped[datetime | None] = mapped_column(UTCDateTime)
    dm_key: Mapped[str | None] = mapped_column(String(64), unique=True)


class ChannelMember(Base):
    __tablename__ = "channel_members"

    channel_id: Mapped[int] = mapped_column(ForeignKey("channels.id", ondelete="CASCADE"), primary_key=True)
    user_id: Mapped[int] = mapped_column(ForeignKey("users.id", ondelete="CASCADE"), primary_key=True, index=True)
    role: Mapped[str] = mapped_column(String(16), default="member")
    joined_at: Mapped[datetime] = mapped_column(UTCDateTime, default=utcnow)
    last_read_seq: Mapped[int] = mapped_column(BigInteger, default=0)
    muted: Mapped[bool] = mapped_column(Boolean, default=False)


class Message(Base):
    __tablename__ = "messages"
    __table_args__ = (
        UniqueConstraint("channel_id", "seq"),
        UniqueConstraint("sender_id", "client_msg_id"),
    )

    id: Mapped[int] = mapped_column(BigIntPK, primary_key=True)
    channel_id: Mapped[int] = mapped_column(ForeignKey("channels.id", ondelete="CASCADE"))
    seq: Mapped[int] = mapped_column(BigInteger)
    sender_id: Mapped[int] = mapped_column(ForeignKey("users.id"))
    # Kept after a recall for audit; the API blanks it once deleted_at is set.
    content: Mapped[str] = mapped_column(Text, default="")
    client_msg_id: Mapped[str | None] = mapped_column(String(64))
    created_at: Mapped[datetime] = mapped_column(UTCDateTime, default=utcnow)
    edited_at: Mapped[datetime | None] = mapped_column(UTCDateTime)
    deleted_at: Mapped[datetime | None] = mapped_column(UTCDateTime)


class MessageMention(Base):
    __tablename__ = "message_mentions"
    __table_args__ = (Index("ix_message_mentions_user_channel_seq", "user_id", "channel_id", "seq"),)

    message_id: Mapped[int] = mapped_column(ForeignKey("messages.id", ondelete="CASCADE"), primary_key=True)
    user_id: Mapped[int] = mapped_column(ForeignKey("users.id", ondelete="CASCADE"), primary_key=True)
    channel_id: Mapped[int] = mapped_column(BigInteger)
    seq: Mapped[int] = mapped_column(BigInteger)


class Blob(Base):
    __tablename__ = "blobs"

    sha256: Mapped[str] = mapped_column(String(64), primary_key=True)
    size: Mapped[int] = mapped_column(BigInteger)
    storage_key: Mapped[str] = mapped_column(String(512))
    created_at: Mapped[datetime] = mapped_column(UTCDateTime, default=utcnow)


class Attachment(Base):
    __tablename__ = "attachments"

    id: Mapped[int] = mapped_column(BigIntPK, primary_key=True)
    blob_sha256: Mapped[str] = mapped_column(ForeignKey("blobs.sha256"))
    filename: Mapped[str] = mapped_column(String(255))
    content_type: Mapped[str] = mapped_column(String(255))
    size: Mapped[int] = mapped_column(BigInteger)
    uploader_id: Mapped[int] = mapped_column(ForeignKey("users.id"))
    message_id: Mapped[int | None] = mapped_column(ForeignKey("messages.id", ondelete="SET NULL"), index=True)
    created_at: Mapped[datetime] = mapped_column(UTCDateTime, default=utcnow)


class Event(Base):
    """Append-only change log. Ids are allocated in commit order (see events.emit)."""

    __tablename__ = "events"
    __table_args__ = (
        Index("ix_events_channel_id_id", "channel_id", "id"),
        Index("ix_events_user_id_id", "user_id", "id"),
    )

    id: Mapped[int] = mapped_column(BigIntPK, primary_key=True)
    type: Mapped[str] = mapped_column(String(32))
    channel_id: Mapped[int | None] = mapped_column(BigInteger)
    # When set, the event is private to this user regardless of channel_id.
    user_id: Mapped[int | None] = mapped_column(BigInteger)
    payload: Mapped[dict[str, Any]] = mapped_column(JSONType)
    created_at: Mapped[datetime] = mapped_column(UTCDateTime, default=utcnow)


__all__ = [
    "Attachment",
    "Blob",
    "Channel",
    "ChannelMember",
    "Event",
    "Message",
    "MessageMention",
    "ServerMeta",
    "Session",
    "User",
    "UserIdentity",
]
