from __future__ import annotations

import asyncio
import contextlib
import logging
import time

from fastapi import APIRouter, WebSocket, WebSocketDisconnect
from pydantic import ValidationError
from sqlalchemy import select

from wittychat_server import models
from wittychat_server.broadcast import Connection
from wittychat_server.events import events_since
from wittychat_server.security import resolve_token
from wittychat_shared import protocol as p

log = logging.getLogger(__name__)
router = APIRouter()

HELLO_TIMEOUT = 10
IDLE_TIMEOUT = 90  # clients ping every 30s
BACKLOG_LIMIT = 1000
TYPING_INTERVAL = 3.0


@router.websocket("/ws")
async def websocket_endpoint(ws: WebSocket) -> None:
    state = ws.app.state
    await ws.accept()
    try:
        hello = p.client_frame_adapter.validate_json(await asyncio.wait_for(ws.receive_text(), HELLO_TIMEOUT))
        if not isinstance(hello, p.WsHello):
            raise ValueError("expected hello")
    except (TimeoutError, ValueError, ValidationError, WebSocketDisconnect):
        with contextlib.suppress(Exception):
            await ws.close(code=4400)
        return

    async with state.sessionmaker() as db:
        resolved = await resolve_token(db, hello.token, state.settings)
    if resolved is None:
        await ws.send_text(p.WsError(code="unauthorized").model_dump_json())
        await ws.close(code=4401)
        return
    user = resolved[1]

    conn = Connection(ws, user.id)
    state.hub.add(conn)  # live events buffer from here on, so nothing slips between backlog and live
    writer = asyncio.create_task(_write(conn))
    try:
        async with state.sessionmaker() as db:
            backlog = await events_since(db, user.id, hello.cursor, limit=BACKLOG_LIMIT)
        if backlog.reset or backlog.has_more:
            conn.push_frame(p.WsResync(reset=backlog.reset))
            conn.close()
            await writer
            return
        for event in backlog.events:
            conn.send_raw(p.WsEvent(event=event).model_dump_json())
        conn.send_raw(p.WsReady(cursor=backlog.cursor).model_dump_json())
        conn.go_live(backlog.cursor)
        await _receive_loop(ws, conn, state)
    except (WebSocketDisconnect, TimeoutError, RuntimeError):
        pass
    finally:
        state.hub.remove(conn)
        conn.close()
        writer.cancel()
        with contextlib.suppress(asyncio.CancelledError, Exception):
            await writer


async def _write(conn: Connection) -> None:
    try:
        await conn.writer()
    finally:
        with contextlib.suppress(Exception):
            await conn.ws.close()


async def _receive_loop(ws: WebSocket, conn: Connection, state) -> None:
    last_typing: dict[int, float] = {}
    while not conn.closed:
        raw = await asyncio.wait_for(ws.receive_text(), IDLE_TIMEOUT)
        try:
            frame = p.client_frame_adapter.validate_json(raw)
        except ValidationError:
            conn.push_frame(p.WsError(code="bad_frame"))
            continue
        if isinstance(frame, p.WsPing):
            conn.push_frame(p.WsPong())
        elif isinstance(frame, p.WsTyping):
            now = time.monotonic()
            if now - last_typing.get(frame.channel_id, 0) < TYPING_INTERVAL:
                continue
            last_typing[frame.channel_id] = now
            async with state.sessionmaker() as db:
                members = set(
                    await db.scalars(
                        select(models.ChannelMember.user_id).where(models.ChannelMember.channel_id == frame.channel_id)
                    )
                )
                if conn.user_id in members:
                    await state.dispatcher.publish_ephemeral(
                        db,
                        sorted(members - {conn.user_id}),
                        p.WsTypingNotice(channel_id=frame.channel_id, user_id=conn.user_id),
                    )
