from __future__ import annotations

from fastapi import APIRouter, Request, Response
from sqlalchemy.exc import IntegrityError

from wittychat_server import chat
from wittychat_server.deps import DB, CurrentUser, commit
from wittychat_server.serializers import message_out
from wittychat_shared import protocol as p

router = APIRouter(tags=["messages"])


@router.post("/channels/{channel_id}/messages", response_model=p.Message, status_code=201)
async def send(
    channel_id: int, req: p.SendMessageRequest, request: Request, response: Response, db: DB, user: CurrentUser
) -> p.Message:
    channel = await chat.get_channel(db, channel_id)
    try:
        message, created = await chat.send_message(db, user, channel, req)
        out = await message_out(db, message)
        await commit(request, db)
    except IntegrityError:
        # Concurrent retry with the same client_msg_id committed first.
        await db.rollback()
        channel = await chat.get_channel(db, channel_id)
        message, created = await chat.send_message(db, user, channel, req)
        out = await message_out(db, message)
    if not created:
        response.status_code = 200
    return out


@router.patch("/messages/{message_id}", response_model=p.Message)
async def edit(message_id: int, req: p.EditMessageRequest, request: Request, db: DB, user: CurrentUser) -> p.Message:
    message = await chat.edit_message(db, user, message_id, req.content)
    out = await message_out(db, message)
    await commit(request, db)
    return out


@router.delete("/messages/{message_id}", response_model=p.Message)
async def delete(message_id: int, request: Request, db: DB, user: CurrentUser) -> p.Message:
    message = await chat.delete_message(db, user, message_id)
    out = await message_out(db, message)
    await commit(request, db)
    return out
