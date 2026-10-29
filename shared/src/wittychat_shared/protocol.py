from __future__ import annotations

from datetime import datetime
from enum import StrEnum
from typing import Annotated, Any, Literal

from pydantic import BaseModel, ConfigDict, Field, TypeAdapter

PROTOCOL_VERSION = 1

MAX_MESSAGE_LENGTH = 20_000
MAX_CLIENT_MSG_ID_LENGTH = 64


class Model(BaseModel):
    model_config = ConfigDict(from_attributes=True)


class ChannelType(StrEnum):
    PUBLIC = "public"
    PRIVATE = "private"
    DM = "dm"


class MemberRole(StrEnum):
    OWNER = "owner"
    MEMBER = "member"


class User(Model):
    id: int
    username: str
    display_name: str
    email: str | None = None
    avatar_url: str | None = None
    is_active: bool = True
    is_admin: bool = False


class Attachment(Model):
    id: int
    filename: str
    content_type: str
    size: int
    sha256: str


class Message(Model):
    id: int
    channel_id: int
    seq: int
    sender_id: int
    content: str
    client_msg_id: str | None = None
    mentions: list[int] = []
    attachments: list[Attachment] = []
    created_at: datetime
    edited_at: datetime | None = None
    deleted: bool = False


class Channel(Model):
    id: int
    type: ChannelType
    name: str | None = None
    topic: str = ""
    last_seq: int = 0
    last_message_at: datetime | None = None
    archived: bool = False
    member_ids: list[int] = []
    created_at: datetime


class Membership(Model):
    channel_id: int
    role: MemberRole = MemberRole.MEMBER
    last_read_seq: int = 0
    muted: bool = False
    mention_count: int = 0


class EventType(StrEnum):
    MESSAGE_CREATED = "message.created"
    MESSAGE_UPDATED = "message.updated"
    MESSAGE_DELETED = "message.deleted"
    CHANNEL_UPSERTED = "channel.upserted"
    CHANNEL_REMOVED = "channel.removed"
    MEMBERSHIP_UPDATED = "membership.updated"
    USER_UPSERTED = "user.upserted"


class Event(Model):
    """One entry of the ordered change log a client replays to stay in sync.

    Payloads: message.* -> Message, channel.upserted -> Channel,
    channel.removed -> {"channel_id": int}, membership.updated -> Membership,
    user.upserted -> User.
    """

    id: int
    type: EventType
    channel_id: int | None = None
    payload: dict[str, Any]
    created_at: datetime


# ---------------------------------------------------------------- REST: auth


class AuthConfig(Model):
    oidc_enabled: bool
    issuer: str | None = None
    client_id: str | None = None
    authorization_endpoint: str | None = None
    scopes: str | None = None
    dev_login: bool = False


class OidcExchangeRequest(Model):
    code: str
    code_verifier: str = Field(min_length=43, max_length=128)
    redirect_uri: str
    nonce: str


class DevLoginRequest(Model):
    username: str = Field(min_length=1, max_length=64, pattern=r"^[A-Za-z0-9_.\-]+$")
    display_name: str | None = Field(default=None, max_length=128)


class LoginResponse(Model):
    token: str
    user: User


# ---------------------------------------------------------------- REST: sync


class Snapshot(Model):
    instance_id: str
    cursor: int
    me: User
    users: list[User]
    channels: list[Channel]
    memberships: list[Membership]


class SyncResponse(Model):
    events: list[Event]
    cursor: int
    has_more: bool = False
    reset: bool = False


# ------------------------------------------------------- REST: channels / messages


class CreateChannelRequest(Model):
    type: Literal[ChannelType.PUBLIC, ChannelType.PRIVATE]
    name: str = Field(min_length=1, max_length=80)
    topic: str = Field(default="", max_length=250)
    member_ids: list[int] = []


class UpdateChannelRequest(Model):
    name: str | None = Field(default=None, min_length=1, max_length=80)
    topic: str | None = Field(default=None, max_length=250)


class OpenDmRequest(Model):
    user_id: int


class AddMembersRequest(Model):
    user_ids: list[int] = Field(min_length=1)


class UpdateMembershipRequest(Model):
    muted: bool


class MarkReadRequest(Model):
    seq: int = Field(ge=0)


class BrowseChannel(Model):
    channel: Channel
    member_count: int
    joined: bool


class SendMessageRequest(Model):
    client_msg_id: str = Field(min_length=1, max_length=MAX_CLIENT_MSG_ID_LENGTH)
    content: str = Field(default="", max_length=MAX_MESSAGE_LENGTH)
    attachment_ids: list[int] = []


class EditMessageRequest(Model):
    content: str = Field(min_length=1, max_length=MAX_MESSAGE_LENGTH)


class HistoryResponse(Model):
    messages: list[Message]
    has_more: bool


# ---------------------------------------------------------------- WebSocket frames


class WsHello(Model):
    type: Literal["hello"] = "hello"
    token: str
    cursor: int = 0
    protocol: int = PROTOCOL_VERSION


class WsPing(Model):
    type: Literal["ping"] = "ping"


class WsTyping(Model):
    type: Literal["typing"] = "typing"
    channel_id: int


ClientFrame = Annotated[WsHello | WsPing | WsTyping, Field(discriminator="type")]
client_frame_adapter: TypeAdapter[WsHello | WsPing | WsTyping] = TypeAdapter(ClientFrame)


class WsReady(Model):
    type: Literal["ready"] = "ready"
    cursor: int


class WsEvent(Model):
    type: Literal["event"] = "event"
    event: Event


class WsPong(Model):
    type: Literal["pong"] = "pong"


class WsTypingNotice(Model):
    type: Literal["typing"] = "typing"
    channel_id: int
    user_id: int


class WsResync(Model):
    """Backlog too large or cursor unusable: client must re-sync over REST and reconnect."""

    type: Literal["resync"] = "resync"
    reset: bool = False


class WsError(Model):
    type: Literal["error"] = "error"
    code: str
    message: str = ""


ServerFrame = Annotated[
    WsReady | WsEvent | WsPong | WsTypingNotice | WsResync | WsError, Field(discriminator="type")
]
server_frame_adapter: TypeAdapter[WsReady | WsEvent | WsPong | WsTypingNotice | WsResync | WsError] = (
    TypeAdapter(ServerFrame)
)
