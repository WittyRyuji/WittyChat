"""Realtime fan-out.

Every committed event is delivered by the Dispatcher of each worker process, which tails
the events table (ids are commit-ordered) and pushes to the WebSocket connections it owns.
Workers are woken by PostgreSQL LISTEN/NOTIFY, or directly via poke() in single-process mode.
"""

from __future__ import annotations

import asyncio
import json
import logging
from collections import defaultdict
from collections.abc import Callable, Iterable

from pydantic import BaseModel
from sqlalchemy import select, text
from sqlalchemy.engine import make_url
from sqlalchemy.ext.asyncio import AsyncSession, async_sessionmaker
from starlette.websockets import WebSocket

from wittychat_server import models
from wittychat_server.events import EPHEMERAL_CHANNEL, NOTIFY_CHANNEL, max_event_id
from wittychat_server.serializers import event_out
from wittychat_shared import protocol as p

log = logging.getLogger(__name__)

OUTBOX_LIMIT = 2000


class Connection:
    def __init__(self, ws: WebSocket, user_id: int) -> None:
        self.ws = ws
        self.user_id = user_id
        self.outbox: asyncio.Queue[str | None] = asyncio.Queue(maxsize=OUTBOX_LIMIT)
        self.last_event_id = 0
        self.ready = False
        self.closed = False
        self._pending: list[p.Event] = []

    def push_event(self, event: p.Event) -> None:
        if not self.ready:
            self._pending.append(event)
        elif event.id > self.last_event_id:
            self.last_event_id = event.id
            self.send_raw(p.WsEvent(event=event).model_dump_json())

    def push_frame(self, frame: BaseModel) -> None:
        self.send_raw(frame.model_dump_json())

    def go_live(self, cursor: int) -> None:
        """Called after the backlog up to `cursor` was queued; flush events buffered meanwhile."""
        self.last_event_id = cursor
        self.ready = True
        pending, self._pending = self._pending, []
        for event in sorted(pending, key=lambda e: e.id):
            self.push_event(event)

    def close(self) -> None:
        if self.closed:
            return
        self.closed = True
        try:
            self.outbox.put_nowait(None)  # writer flushes what is queued, then closes the socket
        except asyncio.QueueFull:
            while not self.outbox.empty():
                self.outbox.get_nowait()
            self.outbox.put_nowait(None)

    def send_raw(self, data: str) -> None:
        if self.closed:
            return
        try:
            self.outbox.put_nowait(data)
        except asyncio.QueueFull:
            log.warning("closing slow websocket consumer user=%s", self.user_id)
            self.close()

    async def writer(self) -> None:
        while (data := await self.outbox.get()) is not None:
            await self.ws.send_text(data)


class Hub:
    def __init__(self) -> None:
        self._conns: dict[int, set[Connection]] = defaultdict(set)

    def add(self, conn: Connection) -> None:
        self._conns[conn.user_id].add(conn)

    def remove(self, conn: Connection) -> None:
        conns = self._conns.get(conn.user_id)
        if conns is not None:
            conns.discard(conn)
            if not conns:
                del self._conns[conn.user_id]

    def connections(self, user_ids: Iterable[int] | None = None) -> list[Connection]:
        if user_ids is None:
            return [c for conns in self._conns.values() for c in conns]
        return [c for uid in user_ids for c in self._conns.get(uid, ())]

    def kick(self, user_id: int) -> None:
        for conn in list(self._conns.get(user_id, ())):
            conn.close()

    def online_user_ids(self) -> set[int]:
        return set(self._conns)

    @property
    def connection_count(self) -> int:
        return sum(len(c) for c in self._conns.values())


class Dispatcher:
    def __init__(self, sessionmaker: async_sessionmaker[AsyncSession], hub: Hub, *, use_pg: bool) -> None:
        self.sessionmaker = sessionmaker
        self.hub = hub
        self.use_pg = use_pg
        self.last_id = 0
        self._wake = asyncio.Event()

    async def start(self) -> None:
        async with self.sessionmaker() as db:
            self.last_id = await max_event_id(db)

    def poke(self) -> None:
        self._wake.set()

    async def run(self) -> None:
        while True:
            await self._wake.wait()
            self._wake.clear()
            try:
                await self.drain()
            except Exception:
                log.exception("event dispatch failed")
                await asyncio.sleep(1)
                self._wake.set()

    async def drain(self) -> None:
        async with self.sessionmaker() as db:
            while True:
                rows = (
                    await db.scalars(
                        select(models.Event).where(models.Event.id > self.last_id).order_by(models.Event.id).limit(500)
                    )
                ).all()
                if not rows:
                    return
                channel_ids = {r.channel_id for r in rows if r.user_id is None and r.channel_id is not None}
                members: dict[int, list[int]] = defaultdict(list)
                if channel_ids:
                    for channel_id, user_id in await db.execute(
                        select(models.ChannelMember.channel_id, models.ChannelMember.user_id).where(
                            models.ChannelMember.channel_id.in_(channel_ids)
                        )
                    ):
                        members[channel_id].append(user_id)
                for row in rows:
                    self._deliver(row, members)
                    self.last_id = row.id

    def _deliver(self, row: models.Event, members: dict[int, list[int]]) -> None:
        event = event_out(row)
        if row.user_id is not None:
            recipients: list[int] | None = [row.user_id]
        elif row.channel_id is not None:
            recipients = members.get(row.channel_id, [])
        else:
            recipients = None
        for conn in self.hub.connections(recipients):
            conn.push_event(event)
        if event.type == p.EventType.USER_UPSERTED and not event.payload.get("is_active", True):
            self.hub.kick(event.payload["id"])

    async def publish_ephemeral(self, db: AsyncSession, user_ids: list[int], frame: BaseModel) -> None:
        if self.use_pg:
            payload = json.dumps({"to": user_ids, "frame": frame.model_dump(mode="json")})
            await db.execute(text("SELECT pg_notify(:c, :p)"), {"c": EPHEMERAL_CHANNEL, "p": payload})
            await db.commit()
        else:
            self.deliver_ephemeral(user_ids, frame.model_dump(mode="json"))

    def deliver_ephemeral(self, user_ids: list[int], frame: dict) -> None:
        data = json.dumps(frame)
        for conn in self.hub.connections(user_ids):
            conn.send_raw(data)


class PgListener:
    """Keeps one LISTEN connection per worker; reconnects and re-drains on failure."""

    def __init__(self, database_url: str, dispatcher: Dispatcher) -> None:
        url = make_url(database_url).set(drivername="postgresql")
        self.dsn = url.render_as_string(hide_password=False)
        self.dispatcher = dispatcher

    def _on_ephemeral(self, *args) -> None:
        try:
            data = json.loads(args[-1])
            self.dispatcher.deliver_ephemeral(data["to"], data["frame"])
        except (ValueError, KeyError):
            log.warning("bad ephemeral payload")

    async def run(self) -> None:
        import asyncpg

        on_event: Callable[..., None] = lambda *args: self.dispatcher.poke()  # noqa: E731
        while True:
            conn = None
            try:
                conn = await asyncpg.connect(self.dsn)
                await conn.add_listener(NOTIFY_CHANNEL, on_event)
                await conn.add_listener(EPHEMERAL_CHANNEL, self._on_ephemeral)
                self.dispatcher.poke()
                while not conn.is_closed():
                    await asyncio.sleep(15)
                    await conn.execute("SELECT 1")
            except asyncio.CancelledError:
                raise
            except Exception:
                log.exception("LISTEN connection lost; retrying")
                await asyncio.sleep(2)
            finally:
                if conn is not None and not conn.is_closed():
                    await conn.close()
