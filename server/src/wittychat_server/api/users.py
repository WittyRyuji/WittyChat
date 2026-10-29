from __future__ import annotations

from fastapi import APIRouter, HTTPException
from sqlalchemy import select

from wittychat_server import models
from wittychat_server.deps import DB, CurrentUser
from wittychat_server.serializers import user_out
from wittychat_shared import protocol as p

router = APIRouter(tags=["users"])


@router.get("/users", response_model=list[p.User])
async def list_users(db: DB, user: CurrentUser) -> list[p.User]:
    return [user_out(u) for u in await db.scalars(select(models.User).order_by(models.User.id))]


@router.get("/users/{user_id}", response_model=p.User)
async def get_user(user_id: int, db: DB, user: CurrentUser) -> p.User:
    found = await db.get(models.User, user_id)
    if found is None:
        raise HTTPException(404, "user not found")
    return user_out(found)
